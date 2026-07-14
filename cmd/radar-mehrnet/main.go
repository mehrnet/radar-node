// Command radar-mehrnet is the radar-node CLI. `probe` is a
// standalone one-shot check runner; `agent` fetches jobs from
// radar-api, runs them through the same Checkers, and reports
// results back -- see README.md for the wire contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mehrnet/radar-node/internal/agent"
	"github.com/mehrnet/radar-node/internal/output"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
)

// version is set at build time via:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
//
// goreleaser (.goreleaser.yaml) does this automatically for tagged
// releases; local `go build`/`make build` leaves it as "dev".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "probe":
		err = runProbe(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "init":
		err = runInit(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("radar-mehrnet", version)
		return
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "radar-mehrnet: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-mehrnet:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `radar-mehrnet - config-driven, multi-protocol probe agent

Every prober -- tcp/udp/dns/icmp/http/system included -- is a YAML
module; there is no separate "native" mechanism. The six built-ins
are embedded in this binary and load automatically; "init" writes
them out as real, editable files.

Usage:
  radar-mehrnet probe <target> [flags]
  radar-mehrnet agent [flags]
  radar-mehrnet init [-C path]

probe flags:
  --type string       tcp | udp | dns | icmp | http | system | <module name> (default "tcp")
  --probe string       warm | hard (default "warm")
  --count int          number of probes to run (default 1)
  --timeout duration    per-probe timeout (default "5s")
  --format string       json | csv | table (default "json")
  --param k=v            module-specific parameter, repeatable
                          (tcp: tls,sni,insecure  dns: record,server  http: method)
  --modules-dir path      load/override modules from *.yaml/*.yml here,
                          on top of the embedded defaults

agent flags:
  --api-url string      radar-api base URL (required)
  --api-key string       "node_id:secret" bearer token (required)
  --api-proxy string      proxy for the agent's own radar-api traffic
                          (http://, https://, socks5://, socks5h://)
  --events-interval duration  how often to sync job definitions (default "30s")
  --scheduler-tick duration    how often to check cached jobs for due-ness (default "2s")
  --concurrency int       max probes running at once (default 64)
  --modules-dir path      load/override modules from *.yaml/*.yml here,
                          on top of the embedded defaults

init flags:
  -C path                directory to write the default module files
                          into (default ".") -- refuses to overwrite
                          an existing file unless --force is set
  --force                 overwrite files that already exist at path

Examples:
  radar-mehrnet probe 1.1.1.1:443 --type tcp --param tls=true
  radar-mehrnet probe https://example.com --type http --count 3 --format table
  radar-mehrnet probe 8.8.8.8 --type icmp --count 5
  radar-mehrnet probe self --type system
  radar-mehrnet init -C /etc/radar-mehrnet/modules.d
  radar-mehrnet agent --api-url https://radar-api.mehrnet.com --api-key node_01J...:s3cr3t --modules-dir /etc/radar-mehrnet/modules.d
`)
}

func runProbe(args []string) error {
	var (
		checkType  string
		mode       string
		count      int
		timeout    time.Duration
		format     string
		params     stringMap
		modulesDir string
	)

	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.StringVar(&checkType, "type", "tcp", "")
	fs.StringVar(&mode, "probe", "warm", "")
	fs.IntVar(&count, "count", 1, "")
	fs.DurationVar(&timeout, "timeout", 5*time.Second, "")
	fs.StringVar(&format, "format", "json", "")
	fs.Var(&params, "param", "")
	fs.StringVar(&modulesDir, "modules-dir", "", "")
	// Go's flag package stops parsing at the first non-flag token, so
	// `probe <target> --type tcp` (target first, as documented in
	// usage()) would otherwise leave --type unparsed. Every flag here
	// takes a value, so a simple partition into flag/value pairs vs.
	// positional args is enough to accept flags in either position.
	if err := fs.Parse(partitionFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one target argument")
	}
	target := fs.Arg(0)

	reg, err := registry.Default()
	if err != nil {
		return err
	}
	if err := reg.LoadModules(modulesDir); err != nil {
		return err
	}
	checker, ok := reg.Get(checkType)
	if !ok {
		return fmt.Errorf("unknown --type %q (not a built-in prober or a module in --modules-dir)", checkType)
	}
	probeMode := probe.Mode(mode)
	if probeMode != probe.ModeWarm && probeMode != probe.ModeHard {
		return fmt.Errorf("--probe must be %q or %q, got %q", probe.ModeWarm, probe.ModeHard, mode)
	}
	if count < 1 {
		return errors.New("--count must be >= 1")
	}

	env := probe.Envelope{Ok: true, Results: make([]probe.Result, 0, count)}
	for i := 1; i <= count; i++ {
		result := checker.Check(context.Background(), probe.Options{
			Target:  target,
			Timeout: timeout,
			Mode:    probeMode,
			Seq:     i,
			Params:  params.toParams(),
		})
		if !result.Ok {
			env.Ok = false
		}
		env.Results = append(env.Results, result)
	}

	return output.Write(output.Format(format), os.Stdout, env)
}

func runAgent(args []string) error {
	var (
		apiURL         string
		apiKey         string
		apiProxy       string
		eventsInterval time.Duration
		schedulerTick  time.Duration
		concurrency    int
		modulesDir     string
	)

	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.StringVar(&apiURL, "api-url", "", "")
	fs.StringVar(&apiKey, "api-key", "", "")
	fs.StringVar(&apiProxy, "api-proxy", "", "")
	fs.DurationVar(&eventsInterval, "events-interval", 30*time.Second, "")
	fs.DurationVar(&schedulerTick, "scheduler-tick", 2*time.Second, "")
	fs.IntVar(&concurrency, "concurrency", 64, "")
	fs.StringVar(&modulesDir, "modules-dir", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if apiURL == "" {
		return errors.New("--api-url is required")
	}
	if apiKey == "" {
		return errors.New("--api-key is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return agent.Run(ctx, agent.Config{
		APIURL:         apiURL,
		APIKey:         apiKey,
		ProxyURL:       apiProxy,
		EventsInterval: eventsInterval,
		SchedulerTick:  schedulerTick,
		Concurrency:    concurrency,
		ModulesDir:     modulesDir,
	})
}

// runInit materializes the embedded default module fixtures
// (registry.DefaultFiles) as real files at path -- the "everything is
// a file, here's a starting point you can edit" entry point. Refuses
// to clobber a file that already exists unless --force is set.
func runInit(args []string) error {
	var (
		path  string
		force bool
	)
	flagSet := flag.NewFlagSet("init", flag.ContinueOnError)
	flagSet.StringVar(&path, "C", ".", "")
	flagSet.BoolVar(&force, "force", false, "")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	entries, err := fs.ReadDir(registry.DefaultFiles, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := fs.ReadFile(registry.DefaultFiles, e.Name())
		if err != nil {
			return err
		}
		dest := filepath.Join(path, e.Name())
		if !force {
			if _, err := os.Stat(dest); err == nil {
				fmt.Fprintf(os.Stderr, "radar-mehrnet init: skipping %s (already exists, use --force to overwrite)\n", dest)
				continue
			}
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		fmt.Println(dest)
	}
	return nil
}

// partitionFlags reorders args so all -flag/--flag tokens (and the
// value that follows them, when not given as -flag=value) come
// before any positional arguments, in original relative order.
func partitionFlags(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// stringMap implements flag.Value for repeatable --param k=v flags.
type stringMap map[string]string

func (m stringMap) String() string {
	if m == nil {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (m *stringMap) Set(value string) error {
	k, v, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("--param must be key=value, got %q", value)
	}
	if *m == nil {
		*m = stringMap{}
	}
	(*m)[k] = v
	return nil
}

// toParams widens stringMap (map[string]string, all the CLI's --param
// flag can express) into probe.Options.Params' map[string]any --
// native checks read the same strings back out via Options.Param().
func (m stringMap) toParams() map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
