package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestSelfUpdateCommandFor_UsesSystemdRunUnitOnLinuxWhenAvailable(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "systemd-run" {
			return "/usr/bin/systemd-run", nil
		}
		return "", errors.New("not found")
	}
	cmd := selfUpdateCommandFor("linux", 0, 4242, lookPath, "curl ... | sh")
	if !strings.HasSuffix(cmd.Path, "systemd-run") {
		t.Fatalf("expected systemd-run as the command, got %q", cmd.Path)
	}
	if !hasArgWithPrefix(cmd.Args, "--unit=radar-node-selfupdate-4242") {
		t.Fatalf("expected a --unit= naming this pid, got %v", cmd.Args)
	}
	if containsArg(cmd.Args, "--scope") {
		t.Fatalf("expected --unit=, not --scope (a scope races this process's own exit) -- got %v", cmd.Args)
	}
	if containsArg(cmd.Args, "--user") {
		t.Fatalf("root (euid 0) shouldn't get --user, got %v", cmd.Args)
	}
}

func TestSelfUpdateCommandFor_AddsUserFlagWhenNotRoot(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/systemd-run", nil }
	cmd := selfUpdateCommandFor("linux", 1000, 4242, lookPath, "curl ... | sh")
	if !containsArg(cmd.Args, "--user") {
		t.Fatalf("expected --user for a non-root euid, got %v", cmd.Args)
	}
}

func TestSelfUpdateCommandFor_FallsBackToPlainShellWhenSystemdRunMissing(t *testing.T) {
	lookPath := func(name string) (string, error) { return "", errors.New("not found") }
	cmd := selfUpdateCommandFor("linux", 0, 4242, lookPath, "curl ... | sh")
	if !strings.HasSuffix(cmd.Path, "sh") || strings.Contains(cmd.Path, "systemd-run") {
		t.Fatalf("expected a plain sh fallback, got %q", cmd.Path)
	}
}

func TestSelfUpdateCommandFor_NeverUsesSystemdRunOffLinux(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/systemd-run", nil }
	cmd := selfUpdateCommandFor("darwin", 0, 4242, lookPath, "curl ... | sh")
	if strings.Contains(cmd.Path, "systemd-run") {
		t.Fatalf("expected no systemd-run outside linux, got %q", cmd.Path)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasArgWithPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
