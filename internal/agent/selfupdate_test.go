package agent

import (
	"errors"
	"path/filepath"
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
	launch := selfUpdateCommandFor("linux", 0, 4242, lookPath, false, "curl ... | sh")
	cmd := launch.cmd
	if launch.detached {
		t.Fatal("systemd-run should not be marked detached")
	}
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
	launch := selfUpdateCommandFor("linux", 1000, 4242, lookPath, false, "curl ... | sh")
	if !containsArg(launch.cmd.Args, "--user") {
		t.Fatalf("expected --user for a non-root euid, got %v", launch.cmd.Args)
	}
}

func TestSelfUpdateCommandFor_FallsBackToPlainShellWhenSystemdRunMissing(t *testing.T) {
	lookPath := func(name string) (string, error) { return "", errors.New("not found") }
	launch := selfUpdateCommandFor("linux", 0, 4242, lookPath, false, "curl ... | sh")
	if launch.detached {
		t.Fatal("plain fallback should not be detached")
	}
	if !strings.HasSuffix(launch.cmd.Path, "sh") || strings.Contains(launch.cmd.Path, "systemd-run") {
		t.Fatalf("expected a plain sh fallback, got %q", launch.cmd.Path)
	}
}

func TestSelfUpdateCommandFor_ProcdWaitsOutRespawnAndStopsService(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/systemd-run", nil }
	installCmd := "curl -fsSL installer | sh -s -- --node_id=node_x"
	launch := selfUpdateCommandFor("linux", 0, 4242, lookPath, true, installCmd)
	if !launch.detached {
		t.Fatal("procd updater must be detached")
	}
	if filepath.Base(launch.cmd.Path) != "sh" {
		t.Fatalf("expected sh, got %q", launch.cmd.Path)
	}
	script := strings.Join(launch.cmd.Args, " ")
	if !strings.Contains(script, "sleep 6") {
		t.Fatalf("expected a sleep past procd's respawn window, got %q", script)
	}
	if !strings.Contains(script, procdInitScript+" stop") {
		t.Fatalf("expected the updater to stop the service itself, got %q", script)
	}
	if !strings.Contains(script, installCmd) {
		t.Fatalf("expected the install command in the wrapper, got %q", script)
	}
	if !strings.Contains(script, "sleep 1") {
		t.Fatalf("expected a settle pause after stop, got %q", script)
	}
}

func TestSelfUpdateCommandFor_NeverUsesSystemdRunOffLinux(t *testing.T) {
	lookPath := func(name string) (string, error) { return "/usr/bin/systemd-run", nil }
	launch := selfUpdateCommandFor("darwin", 0, 4242, lookPath, true, "curl ... | sh")
	if launch.detached {
		t.Fatal("non-linux updater should not be detached")
	}
	if strings.Contains(launch.cmd.Path, "systemd-run") {
		t.Fatalf("expected no systemd-run outside linux, got %q", launch.cmd.Path)
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
