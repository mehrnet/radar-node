package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBuildInstallCommand_NoProxy_OmitsBothProxyFlags(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", nil)
	if strings.Contains(cmd, "--proxy") {
		t.Fatalf("expected no --proxy anywhere without a configured proxy, got %q", cmd)
	}
}

// The bug this guards against: the outer curl that fetches install.sh
// itself was never proxied, only the argument install.sh gets handed
// once it's already running -- a node whose only route to the
// internet is through this proxy could never even download the
// script.
func TestBuildInstallCommand_WithProxy_AppliesToBothTheOuterCurlAndTheScriptArg(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "socks5h://127.0.0.1:1080", nil)

	if !strings.Contains(cmd, "curl -fsSL --proxy socks5h://127.0.0.1:1080 ") {
		t.Fatalf("expected the outer curl fetching install.sh to carry --proxy before the script URL, got %q", cmd)
	}
	if !strings.Contains(cmd, "--proxy=socks5h://127.0.0.1:1080") {
		t.Fatalf("expected install.sh's own --proxy= argument to still be present, got %q", cmd)
	}
}

func TestBuildInstallCommand_IncludesExtraFlags(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", []string{"--install-module=xray", "--remove-module=wireguard"})
	if !strings.Contains(cmd, `sh "$_script" -- --node_id=node_x --api_key=secret --api_url=https://radar-api.example.com --install-module=xray --remove-module=wireguard`) {
		t.Fatalf("expected extra flags appended to the script invocation, got %q", cmd)
	}
}

// Regression test for the real production incident this whole retry
// loop exists for: a node whose outer curl fetch of install.sh itself
// dropped mid-TLS-handshake used to feed sh an empty script -- not an
// error to sh at all, so the self-update silently no-op'd (exit 0,
// zero output) while the old process had already exited, leaving the
// service dead with no local signal anything went wrong. The fetch
// must now retry up to installFetchMaxAttempts and, if every attempt
// fails, exit non-zero -- loud, not silent.
func TestBuildInstallCommand_RetriesTheOuterFetchAndFailsLoudlyIfExhausted(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", nil)

	if !strings.Contains(cmd, "-o \"$_script\"") {
		t.Fatalf("expected install.sh to be downloaded to a real file, not piped straight into sh, got %q", cmd)
	}
	if !strings.Contains(cmd, fmt.Sprintf(`"$_attempt" -ge %d`, installFetchMaxAttempts)) {
		t.Fatalf("expected a retry loop bounded by installFetchMaxAttempts (%d), got %q", installFetchMaxAttempts, cmd)
	}
	if !strings.Contains(cmd, "exit 1") {
		t.Fatalf("expected exhausting every fetch attempt to exit non-zero (loud failure), got %q", cmd)
	}
	if !strings.HasSuffix(cmd, "exit $_status") {
		t.Fatalf("expected the script's own exit status to propagate out, got %q", cmd)
	}
}

// buildInstallCommand builds a multi-line shell script via string
// templating -- the one thing that kind of template can't guarantee on
// its own is that the result actually parses as shell at all. `sh -n`
// is a cheap, direct check for that, catching a broken template here
// instead of it only surfacing much later as a cryptic runtime error
// on a real node.
func TestBuildInstallCommand_GeneratesValidShellSyntax(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "socks5h://127.0.0.1:1080", []string{"--install-module=xray"})
	f, err := os.CreateTemp("", "syntax-check-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(cmd); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("sh", "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("generated command has invalid shell syntax: %v\n%s\ncmd was:\n%s", err, out, cmd)
	}
}
