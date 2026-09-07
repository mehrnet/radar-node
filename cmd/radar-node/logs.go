package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// runLogs reads this node's own journal back out and prints it in a
// form meant for a person.
//
// It shells out to journalctl rather than keeping a log file of its
// own. The agent writes structured JSON to stderr and the service
// manager already captures, rotates and retains it -- a second copy
// written by the agent would double the disk cost, need its own
// rotation, and disagree with the first one whenever the process died
// badly. This is a reader, not a store.
//
// Root installs get a system unit and user installs get a user unit
// (see install.sh), so both are tried; whichever has the unit answers.
func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	lines := fs.Int("n", 50, "how many entries to show")
	follow := fs.Bool("f", false, "stream new entries as they arrive")
	level := fs.String("level", "", "only entries at this level or worse (debug|info|warn|error)")
	src := fs.String("src", "", "only entries from this subsystem (e.g. agent)")
	raw := fs.Bool("raw", false, "print the original JSON lines instead of formatting them")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cmd, err := journalctl(*lines, *follow)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run journalctl: %w", err)
	}

	minRank := levelRank(*level)
	scanner := bufio.NewScanner(stdout)
	// A stack trace in an error field can be long; the default 64KB
	// token limit would drop exactly the entries worth reading.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Anything that isn't one of our JSON lines still gets shown --
		// systemd's own "Started radar-node" notices, a panic, a
		// pre-journal binary's output. Silently dropping them would
		// hide the messages that matter most when something is wrong.
		var entry map[string]any
		if !strings.HasPrefix(strings.TrimSpace(line), "{") || json.Unmarshal([]byte(line), &entry) != nil {
			fmt.Println(line)
			continue
		}
		if *src != "" && asString(entry["src"]) != *src {
			continue
		}
		if minRank > 0 && levelRank(asString(entry["level"])) < minRank {
			continue
		}
		if *raw {
			fmt.Println(line)
			continue
		}
		fmt.Println(format(entry))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

func journalctl(lines int, follow bool) (*exec.Cmd, error) {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, fmt.Errorf("journalctl not found -- radar-node logs reads the service journal, so it needs systemd. On a machine without it, read the service manager's own logs directly")
	}
	args := []string{"-u", "radar-node", "-o", "cat", "-n", fmt.Sprint(lines)}
	if follow {
		args = append(args, "-f")
	}
	// A user install's unit is not visible to the system journal, so
	// --user is added when this process is not root and a user unit
	// exists. Getting this wrong just means an empty listing, which
	// reads as "no logs" rather than "wrong journal" -- hence checking
	// rather than guessing.
	if os.Geteuid() != 0 {
		if home, err := os.UserHomeDir(); err == nil {
			if _, statErr := os.Stat(home + "/.config/systemd/user/radar-node.service"); statErr == nil {
				args = append([]string{"--user"}, args...)
			}
		}
	}
	return exec.Command(path, args...), nil
}

func levelRank(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return 10
	case "info":
		return 20
	case "warn", "warning":
		return 30
	case "error":
		return 40
	}
	return 0
}

// "07:22:48 ERROR agent  heartbeat failed  err=connection refused"
//
// Fixed-width level and src so the eye can scan a column, then the
// message, then whatever fields the call site attached -- sorted, so
// the same entry always renders the same way rather than in Go's map
// iteration order.
func format(entry map[string]any) string {
	ts := asString(entry["ts"])
	if len(ts) >= 19 {
		ts = ts[11:19] // the time, dropping the date and sub-second part
	}
	level := strings.ToUpper(asString(entry["level"]))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %-10s %s", ts, level, asString(entry["src"]), asString(entry["msg"]))

	keys := make([]string, 0, len(entry))
	for k := range entry {
		switch k {
		case "ts", "level", "src", "msg":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s=%v", k, entry[k])
	}
	return b.String()
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
