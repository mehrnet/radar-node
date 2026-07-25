package moduleinstall_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mehrnet/radar-node/internal/moduleinstall"
)

// buildTarGz packs a single file named binName with the given
// content into a tar.gz archive's bytes -- goreleaser's own flat
// archive layout (see radar-node's .goreleaser.yaml), which is what
// installDependency expects to extract from.
func buildTarGz(t *testing.T, binName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: binName, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, binName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create(binName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newTestServer serves a module YAML (with a binary install: entry
// pointing back at this same server, using {os}/{arch}/{ext}
// placeholders, plus a separate file install: entry with its own full
// URL) plus that binary's archive, checksum, and the file's own
// content -- everything Fetch needs for a real end-to-end run against
// this node's own actual runtime.GOOS/GOARCH.
func newTestServer(t *testing.T, binContent []byte, wireGoos, wireArch string) (*httptest.Server, string) {
	t.Helper()
	ext := "tar.gz"
	if wireGoos == "windows" {
		ext = "zip"
	}
	assetName := fmt.Sprintf("thing_latest_%s_%s.%s", wireGoos, wireArch, ext)

	var archive []byte
	if ext == "zip" {
		archive = buildZip(t, "thing-bin", binContent)
	} else {
		archive = buildTarGz(t, "thing-bin", binContent)
	}
	checksum := sha256Hex(archive)
	wrapperScript := "#!/bin/sh\n# modules=__MODULES_DIR__ tools=__TOOLS_DIR__\n"

	mux := http.NewServeMux()
	var moduleYAML string
	mux.HandleFunc("/modules/thing.yaml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, moduleYAML)
	})
	mux.HandleFunc("/releases/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/releases/"+assetName+".checksum.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksum)
	})
	mux.HandleFunc("/modules/wrapper.sh", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, wrapperScript)
	})
	srv := httptest.NewServer(mux)

	moduleYAML = fmt.Sprintf(`
name: thing
version: "1.0-1"
url: %s/modules/thing.yaml
os: [%s]
arch: [%s]
install:
  - name: thing-bin
    kind: binary
    version: "1.0-1"
    url: %s/releases/thing_latest_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/thing-bin
  - name: wrapper.sh
    kind: file
    url: %s/modules/wrapper.sh
    path: __MODULES_DIR__/wrapper.sh
run:
  command: ["/bin/sh", "__MODULES_DIR__/wrapper.sh"]
collect:
  format: writeout_json
`, srv.URL, wireGoos, wireArch, srv.URL, srv.URL)

	return srv, srv.URL + "/modules/thing.yaml"
}

func TestFetch_DownloadsVerifiesAndInstallsEverything(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	srv, moduleURL := newTestServer(t, []byte("fake binary content"), runtime.GOOS, runtime.GOARCH)
	defer srv.Close()

	if err := moduleinstall.Fetch(context.Background(), cfg, moduleURL); err != nil {
		t.Fatal(err)
	}

	binPath := filepath.Join(cfg.ToolsDir, "thing-bin")
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("expected the binary to be installed at %s: %v", binPath, err)
	}
	if string(binData) != "fake binary content" {
		t.Errorf("unexpected binary content: %q", binData)
	}
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("expected the installed binary to be executable, got mode %v", info.Mode())
	}

	wrapperPath := filepath.Join(cfg.ModulesDir, "wrapper.sh")
	wrapperData, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("expected wrapper.sh to be installed: %v", err)
	}
	if strings.Contains(string(wrapperData), "__MODULES_DIR__") || strings.Contains(string(wrapperData), "__TOOLS_DIR__") {
		t.Errorf("expected placeholders substituted, got %q", wrapperData)
	}
	if !strings.Contains(string(wrapperData), cfg.ModulesDir) || !strings.Contains(string(wrapperData), cfg.ToolsDir) {
		t.Errorf("expected the real resolved dirs in the substituted script, got %q", wrapperData)
	}

	yamlPath := filepath.Join(cfg.ModulesDir, "thing.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("expected the module yaml itself to be written: %v", err)
	}
}

func TestFetch_RejectsUnsupportedPlatformBeforeDownloadingAnything(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	// Declare a platform this test will never actually match.
	srv, moduleURL := newTestServer(t, []byte("fake binary content"), "plan9", "amd64")
	defer srv.Close()

	err := moduleinstall.Fetch(context.Background(), cfg, moduleURL)
	if err == nil {
		t.Fatal("expected an error for an unsupported platform")
	}
	if _, statErr := os.Stat(cfg.ToolsDir); statErr == nil {
		t.Error("expected --tools-dir to never even be created when the platform check fails first")
	}
}

func TestFetch_ChecksumMismatchFailsAndInstallsNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	assetName := fmt.Sprintf("thing_latest_%s_%s.%s", runtime.GOOS, runtime.GOARCH, ext)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("real content"))
	})
	mux.HandleFunc("/releases/"+assetName+".checksum.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "0000000000000000000000000000000000000000000000000000000000000000")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/modules/thing.yaml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `
name: thing
url: %s/modules/thing.yaml
install:
  - name: thing-bin
    url: %s/releases/%s
    path: __TOOLS_DIR__/thing-bin
run:
  command: ["true"]
collect:
  format: writeout_json
`, srv.URL, srv.URL, assetName)
	})
	moduleURL := srv.URL + "/modules/thing.yaml"

	err := moduleinstall.Fetch(context.Background(), cfg, moduleURL)
	if err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected a checksum mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.ToolsDir, "thing-bin")); statErr == nil {
		t.Error("expected the binary to never be written after a checksum mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(cfg.ModulesDir, "thing.yaml")); statErr == nil {
		t.Error("expected the module yaml to never be written after a dependency failed")
	}
}

func TestFetch_DefaultsURLWhenModuleDeclaresNone(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	mux := http.NewServeMux()
	mux.HandleFunc("/modules/bare.yaml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `
name: bare
run:
  command: ["true"]
collect:
  format: writeout_json
`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	moduleURL := srv.URL + "/modules/bare.yaml"

	if err := moduleinstall.Fetch(context.Background(), cfg, moduleURL); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ModulesDir, "bare.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "url: "+moduleURL) {
		t.Errorf("expected the fetched-from URL to be recorded in the written file, got %q", data)
	}
}

func TestInstall_RefetchesUsingTheLocallyRecordedURL(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	srv, moduleURL := newTestServer(t, []byte("v1"), runtime.GOOS, runtime.GOARCH)
	defer srv.Close()

	if err := moduleinstall.Fetch(context.Background(), cfg, moduleURL); err != nil {
		t.Fatal(err)
	}
	// Install by name alone -- no URL passed, it has to come from the
	// module's own YAML already sitting in cfg.ModulesDir.
	if err := moduleinstall.Install(context.Background(), cfg, "thing"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ToolsDir, "thing-bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1" {
		t.Errorf("expected the binary re-fetched via Install to still be correct, got %q", data)
	}
}

func TestInstall_FailsWhenNotLocallyKnown(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}
	if err := moduleinstall.Install(context.Background(), cfg, "never-fetched"); err == nil {
		t.Fatal("expected an error installing a module that was never fetched")
	}
}

func TestRemove_DeletesBinaryFilesAndModuleYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	srv, moduleURL := newTestServer(t, []byte("fake binary content"), runtime.GOOS, runtime.GOARCH)
	defer srv.Close()
	if err := moduleinstall.Fetch(context.Background(), cfg, moduleURL); err != nil {
		t.Fatal(err)
	}

	if err := moduleinstall.Remove(cfg, "thing"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(cfg.ToolsDir, "thing-bin"),
		filepath.Join(cfg.ModulesDir, "wrapper.sh"),
		filepath.Join(cfg.ModulesDir, "thing.yaml"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("expected %s to be removed", p)
		}
	}
}

func TestRemove_FailsWhenNotLocallyKnown(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}
	if err := moduleinstall.Remove(cfg, "never-fetched"); err == nil {
		t.Fatal("expected an error removing a module that was never fetched")
	}
}

// Regression test for the v0.26 production incident: install.sh's own
// legacy substitution rewrites every file it deploys under modules.d,
// module YAML included, so an already-installed module's own Path may
// already be a resolved absolute path rather than the
// __TOOLS_DIR__/__MODULES_DIR__ placeholder form. Remove must still
// be able to find and delete it.
func TestRemove_DeletesAlreadyResolvedAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}
	if err := os.MkdirAll(cfg.ToolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.ModulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(cfg.ToolsDir, "openvpn")
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(cfg.ModulesDir, "openvpn.yaml")
	yaml := fmt.Sprintf(`
name: openvpn
install:
  - name: openvpn
    kind: binary
    url: https://example.com/openvpn_{os}_{arch}.{ext}
    path: %s
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`, binPath)
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := moduleinstall.Remove(cfg, "openvpn"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binPath); err == nil {
		t.Error("expected the binary at its already-resolved absolute path to be removed")
	}
	if _, err := os.Stat(yamlPath); err == nil {
		t.Error("expected the module yaml to be removed")
	}
}

// Regression test for a real production incident: radar.mehrnet.com
// serves its own SPA's index.html (a 200, text/html) for any
// unmatched path instead of a real 404, so a version-pinned asset
// that hasn't finished publishing to a given CDN edge yet looks
// identical to a broken URL at the status-code level. A version-
// pinned dependency (Version set, matching a real production module
// YAML) must retry past that instead of either failing outright or --
// worse -- silently checksum-verifying an HTML error page against a
// checksum sidecar for a completely different (and possibly also-
// stale) response.
func TestFetch_RetriesPastSPAFallbackForVersionPinnedAsset(t *testing.T) {
	dir := t.TempDir()
	cfg := moduleinstall.Config{ModulesDir: filepath.Join(dir, "modules.d"), ToolsDir: filepath.Join(dir, "tools")}

	binContent := []byte("fake binary content")
	archive := buildTarGz(t, "thing-bin", binContent)
	checksum := sha256Hex(archive)
	assetPath := fmt.Sprintf("/releases/thing_1.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	var assetRequests, checksumRequests int
	mux := http.NewServeMux()
	var moduleYAML string
	mux.HandleFunc("/modules/thing.yaml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, moduleYAML)
	})
	mux.HandleFunc(assetPath, func(w http.ResponseWriter, r *http.Request) {
		assetRequests++
		if assetRequests == 1 {
			// Simulate the not-yet-published SPA fallback: 200, but HTML.
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html>...</html>")
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archive)
	})
	mux.HandleFunc(assetPath+".checksum.txt", func(w http.ResponseWriter, r *http.Request) {
		checksumRequests++
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, checksum)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	moduleYAML = fmt.Sprintf(`
name: thing
version: "1.0-1"
url: %s/modules/thing.yaml
os: [%s]
arch: [%s]
install:
  - name: thing-bin
    kind: binary
    version: "1.0"
    url: %s/releases/thing_{version}_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/thing-bin
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`, srv.URL, runtime.GOOS, runtime.GOARCH, srv.URL)

	if err := moduleinstall.Fetch(context.Background(), cfg, srv.URL+"/modules/thing.yaml"); err != nil {
		t.Fatalf("expected Fetch to retry past the SPA fallback and succeed, got: %v", err)
	}
	if assetRequests < 2 {
		t.Errorf("expected at least 2 requests for the asset (one SPA-fallback, one real), got %d", assetRequests)
	}
	binData, err := os.ReadFile(filepath.Join(cfg.ToolsDir, "thing-bin"))
	if err != nil {
		t.Fatalf("expected the binary to be installed: %v", err)
	}
	if string(binData) != string(binContent) {
		t.Errorf("installed binary content = %q, want %q", binData, binContent)
	}
}
