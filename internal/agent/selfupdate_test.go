package agent

import (
	"errors"
	"os"
	"os/exec"
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
	if !strings.Contains(script, "sleep 600") || !strings.Contains(script, procdInitScript+" start") {
		t.Fatalf("expected a watchdog that restarts the service if the installer hangs, got %q", script)
	}
	if !strings.Contains(script, "pidof radar-node") {
		t.Fatalf("expected the watchdog to avoid starting a second agent, got %q", script)
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

// The procd wrapper nests a multi-line installer script inside another
// shell script; string interpolation can produce something that only
// fails at the worst possible moment (mid-update, after the service
// has been stopped). Parse it here instead.
func TestSelfUpdateCommandFor_ProcdWrapperHasValidShellSyntax(t *testing.T) {
	installCmd := buildInstallCommand("node_x", "secret", "https://radar-api.example.com", "", []string{"--install-module=xray"})
	launch := selfUpdateCommandFor("linux", 0, 4242, func(string) (string, error) {
		return "/usr/bin/systemd-run", nil
	}, true, installCmd)
	if len(launch.cmd.Args) != 3 || launch.cmd.Args[1] != "-c" {
		t.Fatalf("expected sh -c <script>, got %v", launch.cmd.Args)
	}
	f, err := os.CreateTemp("", "selfupdate-wrapper-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(launch.cmd.Args[2]); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("procd updater wrapper has invalid shell syntax: %v\n%s\nscript was:\n%s", err, out, launch.cmd.Args[2])
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
