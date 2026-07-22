package agent

import (
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

	if !strings.HasPrefix(cmd, "curl -fsSL --proxy socks5h://127.0.0.1:1080 ") {
		t.Fatalf("expected the outer curl fetching install.sh to carry --proxy before the script URL, got %q", cmd)
	}
	if !strings.Contains(cmd, "--proxy=socks5h://127.0.0.1:1080") {
		t.Fatalf("expected install.sh's own --proxy= argument to still be present, got %q", cmd)
	}
}

func TestBuildInstallCommand_IncludesExtraFlags(t *testing.T) {
	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", []string{"--install-xray", "--remove-wireguard"})
	if !strings.HasSuffix(cmd, "--install-xray --remove-wireguard") {
		t.Fatalf("expected extra flags appended at the end, got %q", cmd)
	}
}
