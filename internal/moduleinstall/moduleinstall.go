// Package moduleinstall implements --fetch-module/--install-module/
// --remove-module: downloading a module's own YAML (and whatever
// remote binaries its install: list declares) and placing everything
// this node's agent needs to load it, all driven by the module's own
// already-parsed schema (internal/module) -- no separate shell
// script, and no separate local state beside the module's own YAML
// once it's installed. That YAML's own recorded url field is exactly
// what a later --install-module <name> re-fetches from; there is
// nothing else to keep in sync.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d", u, resp.StatusCode)
	}
	// 200MB safety cap -- comfortably over any real release asset
	// (the largest today, xray, is ~14MB), just a backstop against a
	// misbehaving/malicious server never closing the connection.
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
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
		if destPath, err := resolveInstallPath(dep.Path, cfg); err == nil {
			_ = os.Remove(destPath)
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

	if dep.IsFile() {
		content, err := fetchURL(ctx, client, assetURL)
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

	assetData, err := fetchURL(ctx, client, assetURL)
	if err != nil {
		return fmt.Errorf("install %q: fetch %s: %w", dep.Name, assetURL, err)
	}

	checksumData, err := fetchURL(ctx, client, assetURL+".checksum.txt")
	if err != nil {
		return fmt.Errorf("install %q: fetch checksum %s: %w", dep.Name, assetURL+".checksum.txt", err)
	}
	expected := strings.TrimSpace(string(checksumData))
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
	return nil
}

// resolveInstallPath substitutes dep.Path's leading __TOOLS_DIR__/ or
// __MODULES_DIR__/ placeholder (already enforced at validate() time --
// see module.InstallDependency's own doc comment) for the real,
// resolved directory, then confirms the result still actually lives
// under that directory -- a defense-in-depth check against a
// dependency name/path containing ".." after substitution, since
// --fetch-module trusts whatever module YAML the operator points it
// at, remote content included.
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
