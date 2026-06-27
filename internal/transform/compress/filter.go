// Package compress runs stream compressors/decompressors as external child
// processes. NBackup stays a thin driver: it pipes bytes through a child and lets
// the proven tool do the
// CPU-heavy work, so compression can be threaded and niced independently of nb
// (in-process compression previously pinned every core).
//
// A scheme is a registered name (zstd, gzip, none) that knows how to build the
// argv for compressing and decompressing. The archive records which scheme
// produced it, so restore reverses the exact transform.
package compress

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/programs"
)

// Options tune a scheme invocation.
type Options struct {
	Program string // override the scheme's default binary (e.g. an absolute path); "" = default
	Level   int    // compression level; 0 = scheme default
	Threads int    // worker threads where supported; 0 = scheme default
	Nice    int    // run the child under `nice -n Nice` for CPU politeness; 0 = no nice
}

// Spec describes a scheme: its archive file extension and how to build the child
// argv. A nil argv builder means "no external process" (the none scheme).
type Spec struct {
	Name           string
	Ext            string // archive extension, e.g. "zst", "gz"; "" for none
	compressArgv   func(o Options) []string
	decompressArgv func(o Options) []string
}

var registry = map[string]Spec{}

func register(s Spec) { registry[s.Name] = s }

func init() {
	register(Spec{
		Name: "zstd", Ext: "zst",
		compressArgv: func(o Options) []string {
			argv := []string{prog(o, "zstd")}
			if o.Level > 0 {
				argv = append(argv, "-"+strconv.Itoa(o.Level))
			}
			if o.Threads > 0 {
				argv = append(argv, "-T"+strconv.Itoa(o.Threads))
			}
			return append(argv, "-c")
		},
		decompressArgv: func(o Options) []string { return []string{prog(o, "zstd"), "-d", "-c"} },
	})
	register(Spec{
		Name: "gzip", Ext: "gz",
		compressArgv: func(o Options) []string {
			argv := []string{prog(o, "gzip")}
			if o.Level > 0 {
				argv = append(argv, "-"+strconv.Itoa(o.Level))
			}
			return append(argv, "-c")
		},
		decompressArgv: func(o Options) []string { return []string{prog(o, "gzip"), "-d", "-c"} },
	})
	register(Spec{Name: "none", Ext: ""}) // identity: no child process
}

func prog(o Options, def string) string {
	if o.Program != "" {
		return o.Program
	}
	return def
}

func spec(scheme string) (Spec, error) {
	s, ok := registry[scheme]
	if !ok {
		return Spec{}, fmt.Errorf("unknown compression scheme %q (known: %s)", scheme, strings.Join(sortedNames(registry), ", "))
	}
	return s, nil
}

// Ext returns the archive file extension for a scheme ("" for none).
func Ext(scheme string) (string, error) {
	s, err := spec(scheme)
	if err != nil {
		return "", err
	}
	return s.Ext, nil
}

// Check verifies the scheme is known and its binary is available on PATH.
func Check(scheme string, o Options) error {
	s, err := spec(scheme)
	if err != nil {
		return err
	}
	if s.compressArgv == nil {
		return nil // none: nothing to run
	}
	bin := s.compressArgv(o)[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("scheme %q needs %q on PATH: %w", scheme, bin, err)
	}
	return nil
}

// CompressCmd returns the compressor as a pipeline stage, or ok=false for the identity
// (none) scheme, which contributes no stage. It lets the unified pipeline run compression
// through any executor (local or a remote client).
func CompressCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.compressArgv }, o)
}

// DecompressCmd returns the decompressor as a pipeline stage (the read-side peer of
// CompressCmd), or ok=false for none.
func DecompressCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.decompressArgv }, o)
}

// Filter returns the scheme as a reversible programs.Filter — Forward compresses, Reverse
// decompresses — for the transform layer to place and chain. The none scheme yields a
// Filter with empty cmds (skipped by the pipeline). It errors only for an unknown scheme.
func Filter(scheme string, o Options) (programs.Filter, error) {
	fwd, _, err := CompressCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	rev, _, err := DecompressCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	return programs.Filter{Name: scheme, Forward: fwd, Reverse: rev}, nil
}

func stageCmd(scheme string, pick func(Spec) func(Options) []string, o Options) (programs.Cmd, bool, error) {
	s, err := spec(scheme)
	if err != nil {
		return programs.Cmd{}, false, err
	}
	build := pick(s)
	if build == nil {
		return programs.Cmd{}, false, nil
	}
	argv := build(o)
	return programs.Cmd{Name: argv[0], Args: argv[1:], Nice: o.Nice}, true, nil
}

// sortedNames returns a registry map's keys sorted, for stable "known: …" errors.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
