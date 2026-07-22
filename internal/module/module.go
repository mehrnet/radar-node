// Package module implements the config-driven prober system: every
// prober, "native" (tcp/udp/dns/icmp/http/system) or fully custom, is
// an operator-authored YAML file -- there is no separate hardcoded
// registry. A module either references a built-in Go implementation
// by name (`action:`, in-process, no subprocess) or defines its own
// prepare/run/collect/teardown lifecycle around an external binary
// (`run:`, xray, sing-box, or anything else), executed via argv
// templates. Exactly one of the two is set per module.
//
// Trust boundary (see README.md): module *definitions* come
// only from local YAML files, read once at process start. A remote
// probe can only *invoke* an already-loaded module by name with typed
// parameters -- it can never introduce a new command or placeholder.
// This package enforces that by rejecting any unrecognized
// {{placeholder}} at load time, before a single remote probe is ever
// processed, and by validating every probe's params against the
// module's declared Request schema before running anything.
package module

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mehrnet/radar-node/internal/action"
)

// Step is one lifecycle stage: an argv command, never a shell
// string, so there is no shell-injection surface regardless of what
// a placeholder resolves to.
type Step struct {
	Command []string      `yaml:"command"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Collect describes how to turn the run step's stdout into a
// probe.Result.
type Collect struct {
	// Format is "writeout_json" (stdout is a single JSON object with
	// at least a latency_ms number; all keys become Result.Extra) or
	// "regex" (Pattern is applied to stdout; the named group
	// "latency_ms" is required, every other named group becomes a
	// string in Result.Extra).
	Format  string `yaml:"format"`
	Pattern string `yaml:"pattern,omitempty"`
}

// Module is one operator-authored prober definition -- the only kind
// there is. `Action` and `Run` are mutually exclusive: exactly one
// must be set, selecting whether this module executes as a native
// in-process Go call or a subprocess lifecycle.
type Module struct {
	Name          string `yaml:"name"`
	Engine        string `yaml:"engine,omitempty"`
	EngineVersion string `yaml:"engine_version,omitempty"`
	// Version/URL are this module *package's* own version and the
	// manifest URL it can be re-fetched from -- distinct from
	// Engine/EngineVersion above (which describe the underlying tool
	// this module wraps, e.g. "xray"/"26.3.27"). Version is free-form
	// (e.g. "26.3.27-1", an upstream-version-plus-our-own-packaging-
	// revision suffix), reported per heartbeat (see registry.
	// ModuleVersions) for the dashboard to show and eventually compare
	// against this same URL's own current copy to offer an update.
	// Both optional -- the embedded tcp/udp/dns/... defaults have
	// neither, they're versioned implicitly with radar-node's own
	// release.
	Version string `yaml:"version,omitempty"`
	URL     string `yaml:"url,omitempty"`
	// OS/Arch restrict which platforms this module can even be
	// installed on at all -- e.g. openvpn/wireguard-go ship linux-only
	// builds. Both empty means no restriction (the embedded tcp/udp/
	// dns/... defaults, or any module with no remote Install
	// dependency to begin with). Checked against runtime.GOOS/GOARCH
	// by --fetch-module/--install-module before downloading anything,
	// and reported to radar-api (see registry.ModuleVersions) so the
	// dashboard can grey out an install this node could never use.
	OS   []string `yaml:"os,omitempty"`
	Arch []string `yaml:"arch,omitempty"`
	// Install is this module's own remote artifacts -- what
	// --fetch-module/--install-module actually download and place. A
	// flat list of separately-declared entries, each its own full URL
	// and Kind ("binary" or "file") -- e.g. xray declares the xray
	// binary plus its two wrapper scripts as three independent entries,
	// each independently versioned.
	Install []InstallDependency `yaml:"install,omitempty"`
	// Action names a built-in implementation from internal/action
	// (e.g. "tcp_connect") to call directly, in-process -- no
	// subprocess, no Prepare/Run/Collect/Teardown.
	Action   string  `yaml:"action,omitempty"`
	Prepare  *Step   `yaml:"prepare,omitempty"`
	Run      *Step   `yaml:"run,omitempty"`
	Collect  Collect `yaml:"collect,omitempty"`
	Teardown *Step   `yaml:"teardown,omitempty"`
	// Request/Response declare this module's data form: which params
	// a probe must/may supply, and what a successful result's Extra
	// carries. Request is enforced before every run (see
	// internal/module/checker.go); Response is declarative/
	// documentation only, validated for well-formedness at load time
	// but not checked against actual output, since a check's own
	// failure modes (partial data, a tool's varying output shape on
	// error) shouldn't be indistinguishable from a request-validation
	// rejection.
	Request  []FieldSchema `yaml:"request,omitempty"`
	Response []FieldSchema `yaml:"response,omitempty"`

	// RawYAML and FileHash are populated by LoadFS/LoadDir from the
	// source file itself, not from any yaml tag -- this is what a
	// heartbeat reports (ProberID:FileHash) and what
	// POST /v1/nodes/modules uploads verbatim when radar-api doesn't
	// recognize a hash yet. FileHash is sha256(RawYAML), hex-encoded.
	RawYAML  string `yaml:"-"`
	FileHash string `yaml:"-"`

	compiledPattern *regexp.Regexp // set by validate, only for Collect.Format == "regex"
}

// InstallDependency is one remote artifact a module needs, as
// declared in its own install: list -- what --fetch-module/
// --install-module download and place. A module with a binary plus
// helper scripts (e.g. xray's prepare/run wrappers) declares one
// entry per artifact, each with its own full URL -- there is no
// implicit "relative to the module's own directory" path, so a
// dependency can be hosted anywhere.
type InstallDependency struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version,omitempty"`
	// Kind is "binary" (default if omitted) or "file". A binary is
	// fetched as a goreleaser-style archive, checksum-verified against
	// its URL's own ".checksum.txt" sidecar, and extracted before being
	// written to Path. A file is fetched as-is (no archive, no
	// checksum sidecar) and written to Path directly -- e.g. a
	// prepare/run wrapper script or a static config a run.command
	// shells out to.
	Kind string `yaml:"kind,omitempty"`
	// URL may contain {os}/{arch}/{ext} placeholders -- see
	// ResolveURL, which substitutes them for a specific platform
	// before anything is actually fetched. This is the *source*;
	// Path below is the destination. Required for every entry, binary
	// or file alike.
	URL string `yaml:"url"`
	// Path is where this dependency actually gets written on disk --
	// the destination side of the source/destination pair alongside
	// URL. Must start with the literal "__TOOLS_DIR__/" or
	// "__MODULES_DIR__/" placeholder (the same convention a fetched
	// file's own *contents* already use, see moduleinstall's
	// substitutePlaceholders), resolved to the real, resolved
	// directory (root vs. non-root installs differ) at install time --
	// never a bare/absolute path, so a module can't be authored to
	// write anywhere else on disk. Required for every entry.
	Path string `yaml:"path"`
}

// IsFile reports whether d is a plain file dependency rather than a
// binary -- the only two kinds InstallDependency supports.
func (d InstallDependency) IsFile() bool {
	return d.Kind == "file"
}

// ResolveURL substitutes {os}/{arch}/{ext} in d.URL for the given
// platform -- ext is "zip" for windows, "tar.gz" otherwise, matching
// goreleaser's own archive format split (see this repo's own
// .goreleaser.yaml).
func (d InstallDependency) ResolveURL(goos, arch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	r := strings.NewReplacer("{os}", goos, "{arch}", arch, "{ext}", ext)
	return r.Replace(d.URL)
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func (m *Module) validate() error {
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q must match %s", m.Name, nameRe.String())
	}
	if err := validateFieldSchema(m.Request, m.Name, "request"); err != nil {
		return err
	}
	if err := validateFieldSchema(m.Response, m.Name, "response"); err != nil {
		return err
	}
	for i, dep := range m.Install {
		if dep.Name == "" {
			return fmt.Errorf("module %q: install[%d]: name is required", m.Name, i)
		}
		if dep.URL == "" {
			return fmt.Errorf("module %q: install[%d] (%s): url is required", m.Name, i, dep.Name)
		}
		if dep.Kind != "" && dep.Kind != "binary" && dep.Kind != "file" {
			return fmt.Errorf("module %q: install[%d] (%s): kind must be \"binary\" or \"file\", got %q", m.Name, i, dep.Name, dep.Kind)
		}
		if !strings.HasPrefix(dep.Path, "__TOOLS_DIR__/") && !strings.HasPrefix(dep.Path, "__MODULES_DIR__/") {
			return fmt.Errorf("module %q: install[%d] (%s): path must start with __TOOLS_DIR__/ or __MODULES_DIR__/, got %q", m.Name, i, dep.Name, dep.Path)
		}
	}

	hasAction := m.Action != ""
	hasRun := m.Run != nil && len(m.Run.Command) > 0
	switch {
	case hasAction && hasRun:
		return fmt.Errorf("module %q: action and run.command are mutually exclusive, set only one", m.Name)
	case !hasAction && !hasRun:
		return fmt.Errorf("module %q: must set either action or run.command", m.Name)
	case hasAction:
		if _, ok := action.Get(m.Action); !ok {
			return fmt.Errorf("module %q: unknown action %q", m.Name, m.Action)
		}
		if m.Prepare != nil || m.Teardown != nil {
			return fmt.Errorf("module %q: prepare/teardown only apply to run.command modules, not action", m.Name)
		}
		return nil
	}

	for stepName, step := range map[string]*Step{"prepare": m.Prepare, "run": m.Run, "teardown": m.Teardown} {
		if step == nil {
			continue
		}
		for _, arg := range step.Command {
			if err := validatePlaceholders(arg); err != nil {
				return fmt.Errorf("module %q, %s.command: %w", m.Name, stepName, err)
			}
		}
	}

	switch m.Collect.Format {
	case "writeout_json":
		if m.Collect.Pattern != "" {
			return fmt.Errorf("module %q: collect.pattern is only used with format \"regex\"", m.Name)
		}
	case "regex":
		re, err := regexp.Compile(m.Collect.Pattern)
		if err != nil {
			return fmt.Errorf("module %q: collect.pattern: %w", m.Name, err)
		}
		if !hasGroup(re, "latency_ms") {
			return fmt.Errorf("module %q: collect.pattern must have a named group \"latency_ms\"", m.Name)
		}
		m.compiledPattern = re
	default:
		return fmt.Errorf("module %q: collect.format must be \"writeout_json\" or \"regex\", got %q", m.Name, m.Collect.Format)
	}
	return nil
}

// Manifest is the small, already-validated summary of a module
// uploaded to radar-api via POST /v1/nodes/modules -- deliberately
// not the raw YAML. radar-api parses this with plain JSON.parse
// instead of a YAML parser, which has no anchor/alias expansion
// mechanism at all and therefore no "billion laughs"-style DoS
// surface the way a YAML parser does; the raw YAML source still gets
// uploaded too, but only ever stored as an opaque display string
// server-side, never fed to a parser there.
type Manifest struct {
	Name          string        `json:"name"`
	Engine        string        `json:"engine,omitempty"`
	EngineVersion string        `json:"engine_version,omitempty"`
	Action        string        `json:"action,omitempty"`
	Request       []FieldSchema `json:"request,omitempty"`
	Response      []FieldSchema `json:"response,omitempty"`
}

// ToManifest builds m's upload manifest. Only ever called on a
// Module that has already passed validate() (every Module returned
// by LoadFS/LoadDir has), so there's nothing left to check here --
// this is a pure projection of already-trusted-by-this-process data.
func (m Module) ToManifest() Manifest {
	return Manifest{
		Name:          m.Name,
		Engine:        m.Engine,
		EngineVersion: m.EngineVersion,
		Action:        m.Action,
		Request:       m.Request,
		Response:      m.Response,
	}
}

func hasGroup(re *regexp.Regexp, name string) bool {
	for _, n := range re.SubexpNames() {
		if n == name {
			return true
		}
	}
	return false
}

// LoadDir reads every *.yaml/*.yml file in dir (non-recursive,
// sorted by filename for deterministic load order) as one Module
// each. A missing dir is not an error -- returns no modules -- so
// --modules-dir stays optional everywhere it's exposed as a flag.
// Any invalid file fails the whole load -- per the restart-required,
// no-hot-reload design, a bad config must block startup loudly rather
// than silently run with a smaller capability set than the operator
// intended.
func LoadDir(dir string) ([]Module, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return LoadFS(os.DirFS(dir))
}

// LoadFS is LoadDir generalized to any fs.FS -- in particular, an
// embed.FS of default fixtures shipped inside the binary itself, so
// "native" probers can be loaded through the exact same path as
// anything in --modules-dir rather than needing a separate mechanism.
func LoadFS(fsys fs.FS) ([]Module, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	seen := map[string]string{} // module name -> defining file, to catch duplicates
	modules := make([]Module, 0, len(files))
	for _, name := range files {
		m, err := loadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prior, dup := seen[m.Name]; dup {
			return nil, fmt.Errorf("%s: module name %q already defined in %s", name, m.Name, prior)
		}
		seen[m.Name] = name
		modules = append(modules, m)
	}
	return modules, nil
}

func loadFile(fsys fs.FS, name string) (Module, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Module{}, err
	}
	return ParseBytes(data)
}

// ParseBytes parses a single module's raw YAML source directly,
// without going through LoadFS/LoadDir's directory listing -- used by
// --fetch-module, which downloads a module's yaml over HTTP rather
// than reading it from a local directory.
func ParseBytes(data []byte) (Module, error) {
	var m Module
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Module{}, fmt.Errorf("parse yaml: %w", err)
	}
	if err := m.validate(); err != nil {
		return Module{}, err
	}
	m.RawYAML = string(data)
	sum := sha256.Sum256(data)
	m.FileHash = hex.EncodeToString(sum[:])
	return m, nil
}
