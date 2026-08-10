package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(cmd, `sh "$_script" --node_id=node_x --api_key=secret --api_url=https://radar-api.example.com --install-module=xray --remove-module=wireguard`) {
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

// Regression test for a real production incident: switching from
// "curl | sh -s -- ARGS" to "sh $_script ARGS" (see the retry-loop
// change above) dropped the now-meaningless "--" in between, but it
// wasn't actually meaningless -- "-s --" is sh's own idiom for "read
// the script from stdin, then -- ends *sh's own* option parsing" (sh
// consumes and discards that "--" itself), whereas "sh $_script --
// ARGS" hands a real script *file*, and a literal "--" after the
// filename is no longer sh's own option to consume -- it's just
// $1, passed straight through to the script. node.sh's own arg parser
// doesn't understand a bare "--" and rejected it outright
// ("error: unknown argument: -- (see --help)"), breaking every node's
// self-update the moment it tried to upgrade past the version that
// introduced this retry loop -- caught only because a real node's
// self-update got stuck in exactly the "old process already exited,
// nothing brings the service back" state this whole retry loop was
// built to prevent. `sh -n` (see the syntax test below) can't catch
// this -- "--" is syntactically valid shell, just semantically wrong --
// so this actually executes the generated command against a stub
// "install.sh" and inspects the argv it received.
func TestBuildInstallCommand_ScriptReceivesArgsWithoutAStrayDoubleDash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#!/bin/sh\necho \"count=$#\"\nfor a in \"$@\"; do echo \"arg:[$a]\"; done\n")
	}))
	defer srv.Close()

	cmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", []string{"--install-module=xray"})
	cmd = strings.Replace(cmd, installScriptURL, srv.URL, 1)

	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("generated command failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "arg:[--]") {
		t.Fatalf("expected no stray \"--\" argument reaching the script, got:\n%s", out)
	}
	for _, want := range []string{"arg:[--node_id=node_x]", "arg:[--api_key=secret]", "arg:[--install-module=xray]"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("expected %q in the script's own received args, got:\n%s", want, out)
		}
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
