// Package transform holds the shared scaffolding for NBackup's stream-transform
// scheme registries. A transform (compression, encryption) is a family of named
// schemes, each knowing how to build the argv for its forward (encode) and
// reverse (decode) child command; the sub-packages register their schemes here
// and keep everything scheme-specific — extensions, key checks — local.
package transform

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/programs"
)

// Concat declares how independently encoded segments (frames) of one scheme compose
// on read — the per-scheme capability the archive-shape resolver folds (see
// docs/design/archive-shapes.md). It is declared at register() and never negotiated;
// nothing anywhere branches on scheme names, only on this. The order is meaningful
// (None < PerFrame < Full), so "at least per-frame decodable" reads as >= ConcatPerFrame.
type Concat int

const (
	// ConcatNone: whole-stream only — concatenated encodings are not decodable at all.
	// The zero value, so an undeclared scheme is conservatively unframeable.
	ConcatNone Concat = iota
	// ConcatPerFrame: each frame decodes with ONE Reverse invocation per frame (gpg —
	// every real encryption CLI: GnuPG >= 2.2.8 deliberately rejects concatenated
	// messages, see docs/design/ranged-reads.md).
	ConcatPerFrame
	// ConcatFull: concatenated frames decode as ONE stream with a single stock Reverse
	// invocation (gzip, zstd, the identity none scheme).
	ConcatFull
)

// Scheme is one registered transform scheme: a name plus the argv builders for
// its forward and reverse child commands. A nil builder means "no external
// process" — the identity (none) scheme, which contributes no pipeline stage.
// Concat declares the scheme's frame-composition capability.
type Scheme[O any] struct {
	Name    string
	Concat  Concat
	Forward func(O) []string
	Reverse func(O) []string
}

// Registry maps scheme names to Schemes for one transform kind. kind names the
// transform in the unknown-scheme error ("compression", "encryption"); nice
// extracts the CPU-politeness level from the options so a built stage runs
// under `nice -n N`.
type Registry[O any] struct {
	kind    string
	nice    func(O) int
	schemes map[string]Scheme[O]
}

// NewRegistry returns an empty registry for one transform kind.
func NewRegistry[O any](kind string, nice func(O) int) *Registry[O] {
	return &Registry[O]{kind: kind, nice: nice, schemes: map[string]Scheme[O]{}}
}

// Register adds a scheme under its name.
func (r *Registry[O]) Register(s Scheme[O]) { r.schemes[s.Name] = s }

// Concat returns a scheme's declared frame-composition capability.
func (r *Registry[O]) Concat(scheme string) (Concat, error) {
	s, err := r.Lookup(scheme)
	if err != nil {
		return ConcatNone, err
	}
	return s.Concat, nil
}

// Lookup resolves a scheme by name, or fails with the known names.
func (r *Registry[O]) Lookup(scheme string) (Scheme[O], error) {
	s, ok := r.schemes[scheme]
	if !ok {
		return Scheme[O]{}, fmt.Errorf("unknown %s scheme %q (known: %s)", r.kind, scheme, strings.Join(r.sortedNames(), ", "))
	}
	return s, nil
}

// ForwardCmd returns the scheme's forward transform as a pipeline stage, or
// ok=false for the identity (none) scheme, which contributes no stage.
func (r *Registry[O]) ForwardCmd(scheme string, o O) (cmd programs.Cmd, ok bool, err error) {
	return r.stageCmd(scheme, o, func(s Scheme[O]) func(O) []string { return s.Forward })
}

// ReverseCmd returns the scheme's reverse transform as a pipeline stage (the
// read-side peer of ForwardCmd), or ok=false for none.
func (r *Registry[O]) ReverseCmd(scheme string, o O) (cmd programs.Cmd, ok bool, err error) {
	return r.stageCmd(scheme, o, func(s Scheme[O]) func(O) []string { return s.Reverse })
}

// Filter returns the scheme as a reversible programs.Filter for the transform
// layer to place and chain. The none scheme yields a Filter with empty cmds
// (skipped by the pipeline). It errors only for an unknown scheme.
func (r *Registry[O]) Filter(scheme string, o O) (programs.Filter, error) {
	fwd, _, err := r.ForwardCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	rev, _, err := r.ReverseCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	return programs.Filter{Forward: fwd, Reverse: rev}, nil
}

func (r *Registry[O]) stageCmd(scheme string, o O, pick func(Scheme[O]) func(O) []string) (programs.Cmd, bool, error) {
	s, err := r.Lookup(scheme)
	if err != nil {
		return programs.Cmd{}, false, err
	}
	build := pick(s)
	if build == nil {
		return programs.Cmd{}, false, nil
	}
	argv := build(o)
	return programs.Cmd{Name: argv[0], Args: argv[1:], Nice: r.nice(o)}, true, nil
}

// sortedNames returns the registered scheme names sorted, for stable "known: …" errors.
func (r *Registry[O]) sortedNames() []string {
	out := make([]string, 0, len(r.schemes))
	for k := range r.schemes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Prog picks the child binary for a scheme invocation: the per-invocation
// override when set, else the scheme's default.
func Prog(override, def string) string {
	if override != "" {
		return override
	}
	return def
}
