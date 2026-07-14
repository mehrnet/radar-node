// Package registry builds the set of probe.Checkers available to a
// process, and is shared by both the `probe` and `agent` subcommands
// so they always agree on prober names. There is no separate
// hardcoded "native" list -- every prober, including tcp/udp/dns/
// icmp/http/system, is loaded the same way, as a module. What makes
// the shipped six feel built-in is only that Default() loads them
// from fixtures embedded in the binary itself (see defaults/*.yaml);
// `radar-node init` materializes those same files to disk for
// anyone who wants to inspect, fork, or override them.
package registry

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
)

//go:embed defaults/*.yaml
var defaultsFS embed.FS

// DefaultFiles is the embedded default fixtures, exposed for
// `radar-node init` to write out to a real directory.
var DefaultFiles = mustSub(defaultsFS, "defaults")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // unreachable: dir is a compile-time constant matching the go:embed pattern above
	}
	return sub
}

type entry struct {
	checker  probe.Checker
	rawYAML  string
	fileHash string
	manifest module.Manifest
}

type Registry map[string]entry

// Default returns a Registry loaded from the embedded default
// fixtures (tcp/udp/dns/icmp/http/https/system), every one an
// action-based module -- in-process, no subprocess overhead, exactly
// like the old hardcoded native checks.
func Default() (Registry, error) {
	r := Registry{}
	if err := r.loadFS(DefaultFiles); err != nil {
		return nil, fmt.Errorf("load embedded default modules: %w", err)
	}
	return r, nil
}

func (r Registry) add(m module.Module) {
	r[m.Name] = entry{checker: module.NewChecker(m), rawYAML: m.RawYAML, fileHash: m.FileHash, manifest: m.ToManifest()}
}

func (r Registry) loadFS(fsys fs.FS) error {
	modules, err := module.LoadFS(fsys)
	if err != nil {
		return err
	}
	for _, m := range modules {
		r.add(m)
	}
	return nil
}

// LoadModules loads every module in dir (see module.LoadDir) and adds
// each as a Checker, overriding any embedded default of the same
// name -- a user's own tcp.yaml in --modules-dir replaces the
// embedded one rather than conflicting with it. A dir of "" is a
// no-op, not an error, so --modules-dir is optional everywhere it's
// exposed as a flag.
func (r Registry) LoadModules(dir string) error {
	if dir == "" {
		return nil
	}
	modules, err := module.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("load modules from %s: %w", dir, err)
	}
	for _, m := range modules {
		r.add(m)
	}
	return nil
}

// Get returns the Checker registered under name, if any.
func (r Registry) Get(name string) (probe.Checker, bool) {
	e, ok := r[name]
	return e.checker, ok
}

// ProberHashes is every loaded module's "prober_id:file_hash" pair,
// for reporting in a wire.HeartbeatRequest.
func (r Registry) ProberHashes() []string {
	pairs := make([]string, 0, len(r))
	for name, e := range r {
		pairs = append(pairs, name+":"+e.fileHash)
	}
	return pairs
}

// RawYAML returns the source YAML, its content hash, and its parsed
// upload manifest for a loaded module by prober_id, for uploading via
// POST /v1/nodes/modules when the server responds that it doesn't
// recognize this module's current hash yet.
func (r Registry) RawYAML(proberID string) (yamlSrc, fileHash string, manifest module.Manifest, ok bool) {
	e, ok := r[proberID]
	return e.rawYAML, e.fileHash, e.manifest, ok
}
