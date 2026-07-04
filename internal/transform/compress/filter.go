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
	"strconv"

	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform"
)

// Options tune a scheme invocation.
type Options struct {
	Program string // override the scheme's default binary (e.g. an absolute path); "" = default
	Level   int    // compression level; 0 = scheme default
	Threads int    // worker threads where supported; 0 = scheme default
	Nice    int    // run the child under `nice -n Nice` for CPU politeness; 0 = no nice
}

var registry = transform.NewRegistry[Options]("compression", func(o Options) int { return o.Nice })

// exts maps a scheme to its archive file extension ("" for none).
var exts = map[string]string{}

// register adds a scheme: its archive file extension, its frame-composition
// capability, and how to build the child argv. A nil argv builder means "no external
// process" (the none scheme).
func register(name, ext string, concat transform.Concat, compressArgv, decompressArgv func(Options) []string) {
	registry.Register(transform.Scheme[Options]{Name: name, Concat: concat, Forward: compressArgv, Reverse: decompressArgv})
	exts[name] = ext
}

func init() {
	// zstd and gzip are ConcatFull: their formats define concatenated members as ONE
	// stream, and the stock tool decodes it in a single invocation (gzip PoC-verified
	// byte-exact; re-check zstd multistream on a machine WITH zstd before relying on it).
	register("zstd", "zst", transform.ConcatFull,
		func(o Options) []string {
			argv := []string{transform.Prog(o.Program, "zstd")}
			if o.Level > 0 {
				argv = append(argv, "-"+strconv.Itoa(o.Level))
			}
			if o.Threads > 0 {
				argv = append(argv, "-T"+strconv.Itoa(o.Threads))
			}
			return append(argv, "-c")
		},
		func(o Options) []string { return []string{transform.Prog(o.Program, "zstd"), "-d", "-c"} },
	)
	register("gzip", "gz", transform.ConcatFull,
		func(o Options) []string {
			argv := []string{transform.Prog(o.Program, "gzip")}
			if o.Level > 0 {
				argv = append(argv, "-"+strconv.Itoa(o.Level))
			}
			return append(argv, "-c")
		},
		func(o Options) []string { return []string{transform.Prog(o.Program, "gzip"), "-d", "-c"} },
	)
	register("none", "", transform.ConcatFull, nil, nil) // identity: no child process; concatenation is trivially one stream
}

// Ext returns the archive file extension for a scheme ("" for none).
func Ext(scheme string) (string, error) {
	if _, err := registry.Lookup(scheme); err != nil {
		return "", err
	}
	return exts[scheme], nil
}

// Check verifies the scheme is known and its binary is available on PATH.
func Check(scheme string, o Options) error {
	s, err := registry.Lookup(scheme)
	if err != nil {
		return err
	}
	if s.Forward == nil {
		return nil // none: nothing to run
	}
	bin := s.Forward(o)[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("scheme %q needs %q on PATH: %w", scheme, bin, err)
	}
	return nil
}

// CompressCmd returns the compressor as a pipeline stage, or ok=false for the identity
// (none) scheme, which contributes no stage. It lets the unified pipeline run compression
// through any executor (local or a remote client).
func CompressCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return registry.ForwardCmd(scheme, o)
}

// DecompressCmd returns the decompressor as a pipeline stage (the read-side peer of
// CompressCmd), or ok=false for none.
func DecompressCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return registry.ReverseCmd(scheme, o)
}

// Filter returns the scheme as a reversible programs.Filter — Forward compresses, Reverse
// decompresses — for the transform layer to place and chain. The none scheme yields a
// Filter with empty cmds (skipped by the pipeline). It errors only for an unknown scheme.
func Filter(scheme string, o Options) (programs.Filter, error) {
	return registry.Filter(scheme, o)
}

// Concat returns the scheme's declared frame-composition capability.
func Concat(scheme string) (transform.Concat, error) {
	return registry.Concat(scheme)
}
