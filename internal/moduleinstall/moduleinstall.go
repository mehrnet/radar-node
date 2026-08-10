// Package moduleinstall implements --fetch-module/--install-module/
// --remove-module: downloading a module's own YAML (and whatever
// remote binaries its install: list declares) and placing everything
// this node's agent needs to load it, all driven by the module's own
// already-parsed schema (internal/module) -- no separate shell
// script. That YAML's own recorded url field is exactly what a later
// --install-module <name> re-fetches from; there is nothing else to
// keep in sync, beside one small sidecar per installed binary
// dependency (see installDependency's own comment) recording which
// archive checksum it was last verified against, purely so a bare
// re-run that finds nothing has actually changed can skip re-
// downloading it entirely.
package moduleinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/module"
)

// Config holds the directories this package reads/writes into and
// the proxy every fetch goes through -- mirrors install.sh's own
// MODULES_DIR/TOOLS_DIR/PROXY.
type Config struct {
	ModulesDir string
	ToolsDir   string
	ProxyURL   string
}

func (cfg Config) httpClient() (*http.Client, error) {
	transport, err := apiclient.BuildTransport(cfg.ProxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second}, nil
}

// fetchURL is the one place this package reads bytes off the
// network -- module YAML, release binaries, and their .checksum.txt
// sidecars all go through it, so proxy handling only has to be
// correct in one spot.
func fetchURL(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	data, _, err := fetchURLWithType(ctx, client, u)
	return data, err
}

// fetchURLWithType is fetchURL, but also returns the response's
// Content-Type -- see fetchImmutableAsset's own comment on why that,
// not the HTTP status, is what actually distinguishes a real asset
// from radar.mehrnet.com's own SPA fallback page.
func fetchURLWithType(ctx context.Context, client *http.Client, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: unexpected status %d", u, resp.StatusCode)
	}
	// 200MB safety cap -- comfortably over any real release asset
	// (the largest today, xray, is ~14MB), just a backstop against a
	// misbehaving/malicious server never closing the connection.
	body := io.Reader(resp.Body)
	// Only for a response actually worth reporting on -- the module
	// YAML and .checksum.txt sidecars fetchURL/fetchURLWithType also
	// serve are a few hundred bytes at most and complete before a
	// human could ever notice, unlike the real binaries (xray alone
	// is ~14MB) this same function fetches too. A slow or heavily
	// proxied connection could otherwise sit here, silently, for
	// minutes -- observed in production as a genuinely alarming
	// "did this hang?" with zero output the whole time.
	if resp.ContentLength >= progressMinBytes {
		body = &progressReader{Reader: resp.Body, label: filepath.Base(u), total: resp.ContentLength, lastReport: time.Now()}
	}
	data, err := io.ReadAll(io.LimitReader(body, 200<<20))
	return data, resp.Header.Get("Content-Type"), err
}

const progressMinBytes = 1 << 20 // 1MB

// progressReader wraps a response body, printing a periodic "label:
// NN.N%" line to stderr (in place, via \r, matching install.sh's own
// curl --progress-bar convention) as bytes actually come in -- see
// fetchURLWithType's own comment on why this matters. Throttled to
// twice a second rather than on every Read: a fast local connection
// can call Read many times a second, and flushing a terminal line
// that often adds real overhead for no visible benefit.
type progressReader struct {
	io.Reader
	label      string
	total      int64
	read       int64
	lastReport time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.Reader.Read(b)
	p.read += int64(n)
	now := time.Now()
	done := err != nil
	if done || now.Sub(p.lastReport) >= 500*time.Millisecond {
		p.lastReport = now
		pct := float64(p.read) / float64(p.total) * 100
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(os.Stderr, "\r  %s: %.1f%%", p.label, pct)
		if done {
			fmt.Fprintln(os.Stderr)
		}
	}
	return n, err
}

// fetchAssetWithRetry retries fetchURLWithType up to 5 times, same
// backoff shape as install.sh's own shell-side fetch_with_retry/
// run_with_retry, on *any* failure -- a genuine network error (a
// dropped connection, the exact shape observed in production on a
// node behind a flaky proxy) for every dependency this is used for,
// version-pinned or not. When versioned is true, a text/html response
// is treated as one more reason to retry too: that specifically means
// radar.mehrnet.com's own SPA fallback page (its origin serves a 200,
// its own index.html, for any unmatched path rather than a real 404),
// which for a permanently-immutable version-pinned asset (see
// module.InstallDependency's own doc comment on {version}) means "not
// published yet," worth waiting out rather than surfacing as a
// failure immediately. A mutable "_latest_"-style URL has no such
// "not published yet" state to wait out -- it's either there or
// genuinely broken -- so that specific check is skipped when
// versioned is false; a real network error still retries either way.
func fetchAssetWithRetry(ctx context.Context, client *http.Client, u string, versioned bool) ([]byte, error) {
	const maxAttempts = 5
	for attempt := 1; ; attempt++ {
		data, ctype, err := fetchURLWithType(ctx, client, u)
		notPublishedYet := versioned && err == nil && strings.HasPrefix(ctype, "text/html")
		if err == nil && !notPublishedYet {
			return data, nil
		}
		if attempt >= maxAttempts {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("GET %s: still not published after %d attempts", u, maxAttempts)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt*2) * time.Second):
		}
	}
}

// Fetch downloads a module's YAML from moduleURL, checks this node's
// own platform against the module's declared OS/Arch (if any), then
// downloads+verifies+installs every declared install: dependency and
// its sibling Files (fetched from the *module's own* directory, the
// same base path as moduleURL) before finally writing the module
// YAML itself into cfg.ModulesDir -- what makes it "locally known"
// for a later Install/Remove by name. Nothing is written until every
// dependency has been downloaded and checksum-verified.
func Fetch(ctx context.Context, cfg Config, moduleURL string) error {
	client, err := cfg.httpClient()
	if err != nil {
		return err
	}

	data, err := fetchURL(ctx, client, moduleURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", moduleURL, err)
	}
	m, err := module.ParseBytes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", moduleURL, err)
	}
	declaredOwnURL := m.URL != ""
	if !declaredOwnURL {
		// A module authored without its own url: field still needs
		// one recorded locally -- falling back to wherever it was
		// actually fetched from is what lets a later
		// --install-module <name> work at all.
		m.URL = moduleURL
	}
	if err := checkPlatform(m); err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.ToolsDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", cfg.ToolsDir, err)
	}
	if err := os.MkdirAll(cfg.ModulesDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", cfg.ModulesDir, err)
	}

	for _, dep := range m.Install {
		if err := installDependency(ctx, client, cfg, dep); err != nil {
			return fmt.Errorf("module %q: %w", m.Name, err)
		}
	}

	// Written last, only once every dependency above has succeeded --
	// a failure partway through never leaves a module "locally known"
	// without the actual binaries/files it needs.
	rawYAML := data
	if !declaredOwnURL {
		rawYAML = append(append([]byte{}, data...), []byte(fmt.Sprintf("\nurl: %s\n", moduleURL))...)
	}
	yamlPath := filepath.Join(cfg.ModulesDir, m.Name+".yaml")
	if err := os.WriteFile(yamlPath, rawYAML, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", yamlPath, err)
	}
	return nil
}

// Install re-fetches an already-locally-known module by name, using
// its own recorded url field -- there is no separate state to
// consult beside the module's own YAML already sitting in
// cfg.ModulesDir.
func Install(ctx context.Context, cfg Config, name string) error {
	yamlPath := filepath.Join(cfg.ModulesDir, name+".yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("%q is not a locally known module (fetch it first with --fetch-module): %w", name, err)
	}
	m, err := module.ParseBytes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", yamlPath, err)
	}
	if m.URL == "" {
		return fmt.Errorf("%q has no url recorded -- re-fetch it with --fetch-module instead", name)
	}
	return Fetch(ctx, cfg, m.URL)
}

// Remove deletes an already-locally-known module's installed
// binaries, sibling files, and the module YAML itself. Best-effort on
// the binaries/files (a partially-installed module shouldn't block
// cleanup of what did make it to disk); only a missing/unparseable
// module YAML itself is an error.
func Remove(cfg Config, name string) error {
	yamlPath := filepath.Join(cfg.ModulesDir, name+".yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("%q is not a locally known module: %w", name, err)
	}
	m, err := module.ParseBytes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", yamlPath, err)
	}
	for _, dep := range m.Install {
		if destPath, err := resolveExistingPath(dep.Path, cfg); err == nil {
			_ = os.Remove(destPath)
			_ = os.Remove(destPath + ".checksum") // see installDependency's own comment; harmless no-op for a file-kind dep, which never has one
		}
	}
	return os.Remove(yamlPath)
}

func checkPlatform(m module.Module) error {
	if len(m.OS) > 0 && !contains(m.OS, runtime.GOOS) {
		return fmt.Errorf("module %q does not support this OS (%s) -- supports: %v", m.Name, runtime.GOOS, m.OS)
	}
	if len(m.Arch) > 0 && !contains(m.Arch, runtime.GOARCH) {
		return fmt.Errorf("module %q does not support this architecture (%s) -- supports: %v", m.Name, runtime.GOARCH, m.Arch)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// installDependency downloads and places one install: entry. A binary
// (the default Kind) is verified against its asset URL's own
// ".checksum.txt" sidecar (see radar/releases-sync.sh, which is what
// publishes that convention), extracted, and written to Path as an
// executable. A file is fetched as-is -- no archive, no checksum
// sidecar -- placeholder-substituted, and written to Path.
func installDependency(ctx context.Context, client *http.Client, cfg Config, dep module.InstallDependency) error {
	destPath, err := resolveInstallPath(dep.Path, cfg)
	if err != nil {
		return fmt.Errorf("install %q: %w", dep.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("install %q: create %s: %w", dep.Name, filepath.Dir(destPath), err)
	}

	assetURL := dep.ResolveURL(runtime.GOOS, runtime.GOARCH)
	// Every fetch below retries on a real network failure regardless
	// of version-pinning (see fetchAssetWithRetry's own comment) --
	// versioned only changes whether a text/html response also counts
	// as "worth retrying" (a not-yet-published version-pinned asset)
	// or not (a mutable "_latest_"-style URL is never "not published
	// yet," just possibly stale).
	versioned := dep.Version != ""
	fetch := func(ctx context.Context, client *http.Client, u string) ([]byte, error) {
		return fetchAssetWithRetry(ctx, client, u, versioned)
	}

	if dep.IsFile() {
		content, err := fetch(ctx, client, assetURL)
		if err != nil {
			return fmt.Errorf("install %q: fetch %s: %w", dep.Name, assetURL, err)
		}
		content = substitutePlaceholders(content, cfg)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(destPath, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(destPath, content, mode); err != nil {
			return fmt.Errorf("write %s: %w", destPath, err)
		}
		return nil
	}

	// The checksum sidecar first, on its own -- a few dozen bytes,
	// negligible even on a bad connection -- before ever touching the
	// real asset (often 10s of MB). install.sh re-runs Install for
	// every already-opted-in module on every single bare re-run, by
	// design, to catch a real update (see this package's own doc
	// comment); without this short-circuit, that meant re-downloading
	// xray/wireguard/openvpn's full binaries from scratch every time
	// regardless of whether anything had actually changed, which is
	// most of the time in practice -- and painfully slow to boot,
	// silently so, over exactly the kind of degraded/proxied
	// connection this node might be stuck behind.
	checksumURL := assetURL + ".checksum.txt"
	checksumData, err := fetch(ctx, client, checksumURL)
	if err != nil {
		return fmt.Errorf("install %q: fetch checksum %s: %w", dep.Name, checksumURL, err)
	}
	expected := strings.TrimSpace(string(checksumData))

	// markerPath records which archive checksum destPath was last
	// verified against -- not destPath's own hash, which can't be
	// compared against expected directly (expected is the *archive*'s
	// checksum, destPath holds the binary *extracted* from it, never
	// byte-identical to its own container). A match here, with the
	// binary still actually present, means this exact version is
	// already correctly installed: nothing left to do.
	markerPath := destPath + ".checksum"
	if existing, err := os.ReadFile(markerPath); err == nil && strings.TrimSpace(string(existing)) == expected {
		if _, err := os.Stat(destPath); err == nil {
			return nil
		}
	}

	assetData, err := fetch(ctx, client, assetURL)
	if err != nil {
		return fmt.Errorf("install %q: fetch %s: %w", dep.Name, assetURL, err)
	}
	sum := sha256.Sum256(assetData)
	actual := hex.EncodeToString(sum[:])
	if expected != actual {
		return fmt.Errorf("install %q: checksum mismatch for %s (expected %s, got %s)", dep.Name, assetURL, expected, actual)
	}

	binData, err := extractBinary(assetData, dep.Name, strings.HasSuffix(assetURL, ".zip"))
	if err != nil {
		return fmt.Errorf("install %q: %w", dep.Name, err)
	}
	if err := os.WriteFile(destPath, binData, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	if err := os.WriteFile(markerPath, []byte(expected), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", markerPath, err)
	}
	return nil
}

// resolveInstallPath substitutes dep.Path's leading __TOOLS_DIR__/ or
// __MODULES_DIR__/ placeholder for the real, resolved directory, then
// confirms the result still actually lives under that directory -- a
// defense-in-depth check against a dependency name/path containing
// ".." after substitution, since --fetch-module trusts whatever
// module YAML the operator points it at, remote content included.
// Strict on purpose: this is what decides where a *new* download gets
// written, so an already-resolved absolute path is rejected rather
// than trusted -- only resolveExistingPath (Remove's own, more
// permissive counterpart) accepts that form.
func resolveInstallPath(path string, cfg Config) (string, error) {
	var baseDir, rel string
	switch {
	case strings.HasPrefix(path, "__TOOLS_DIR__/"):
		baseDir, rel = cfg.ToolsDir, strings.TrimPrefix(path, "__TOOLS_DIR__/")
	case strings.HasPrefix(path, "__MODULES_DIR__/"):
		baseDir, rel = cfg.ModulesDir, strings.TrimPrefix(path, "__MODULES_DIR__/")
	default:
		return "", fmt.Errorf("path %q must start with __TOOLS_DIR__/ or __MODULES_DIR__/", path)
	}
	full := filepath.Join(baseDir, rel)
	if full != baseDir && !strings.HasPrefix(full, baseDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes its base directory", path)
	}
	return full, nil
}

// resolveExistingPath is resolveInstallPath's permissive counterpart,
// used only by Remove -- which is naming a file to delete, not a new
// one to write, so an already-absolute Path (e.g. install.sh's own
// legacy substitution having already resolved a local copy's
// placeholder before this package ever saw it -- see
// module.InstallDependency.Path's own doc comment) is accepted as-is.
func resolveExistingPath(path string, cfg Config) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	return resolveInstallPath(path, cfg)
}

// substitutePlaceholders resolves __MODULES_DIR__/__TOOLS_DIR__ in a
// fetched wrapper script/config -- the same convention install.sh's
// own sed substitution has always used, so a module's Files can
// reference the real, resolved paths (root vs. non-root installs use
// different directories) without knowing them in advance.
func substitutePlaceholders(data []byte, cfg Config) []byte {
	s := string(data)
	s = strings.ReplaceAll(s, "__MODULES_DIR__", cfg.ModulesDir)
	s = strings.ReplaceAll(s, "__TOOLS_DIR__", cfg.ToolsDir)
	return []byte(s)
}

// extractBinary pulls binName out of a tar.gz or zip archive's bytes
// -- goreleaser's own archive layout (see mehrnet/static-builds and
// radar-node's own .goreleaser.yaml) is always flat, the binary at
// the archive root under this exact name.
func extractBinary(archiveData []byte, binName string, zipFmt bool) ([]byte, error) {
	if zipFmt {
		return extractFromZip(archiveData, binName)
	}
	return extractFromTarGz(archiveData, binName)
}

func extractFromTarGz(data []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if filepath.Base(hdr.Name) == binName {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("archive doesn't contain %q", binName)
}

func extractFromZip(data []byte, binName string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	for _, f := range r.File {
		if filepath.Base(f.Name) == binName {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("archive doesn't contain %q", binName)
}
