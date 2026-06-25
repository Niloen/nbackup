// Package filter runs stream compressors/decompressors as external child
// processes, the way Amanda orchestrates gzip/custom compress. NBackup stays a
// thin driver: it pipes bytes through a child and lets the proven tool do the
// CPU-heavy work, so compression can be threaded and niced independently of nb
// (in-process compression previously pinned every core).
//
// A codec is a registered name (zstd, gzip, none) that knows how to build the
// argv for compressing and decompressing. The archive records which codec
// produced it, so restore reverses the exact transform.
package compress

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/hostexec"
	"github.com/Niloen/nbackup/internal/streamproc"
)

// Options tune a codec invocation.
type Options struct {
	Program string // override the codec's default binary (e.g. an absolute path); "" = default
	Level   int    // compression level; 0 = codec default
	Threads int    // worker threads where supported; 0 = codec default
	Nice    int    // run the child under `nice -n Nice` for CPU politeness; 0 = no nice
}

// Spec describes a codec: its archive file extension and how to build the child
// argv. A nil argv builder means "no external process" (the none codec).
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

func prog(o Options, def string) string { return streamproc.ProgramOr(o.Program, def) }

func spec(codec string) (Spec, error) {
	s, ok := registry[codec]
	if !ok {
		return Spec{}, fmt.Errorf("unknown codec %q (known: %s)", codec, strings.Join(streamproc.SortedNames(registry), ", "))
	}
	return s, nil
}

// Ext returns the archive file extension for a codec ("" for none).
func Ext(codec string) (string, error) {
	s, err := spec(codec)
	if err != nil {
		return "", err
	}
	return s.Ext, nil
}

// Check verifies the codec is known and its binary is available on PATH.
func Check(codec string, o Options) error {
	s, err := spec(codec)
	if err != nil {
		return err
	}
	if s.compressArgv == nil {
		return nil // none: nothing to run
	}
	bin := s.compressArgv(o)[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("codec %q needs %q on PATH: %w", codec, bin, err)
	}
	return nil
}

// Decompress returns a ReadCloser that yields the decompressed form of src by
// piping it through the codec's decompressor child. Close waits the child.
func Decompress(codec string, src io.Reader, o Options) (io.ReadCloser, error) {
	s, err := spec(codec)
	if err != nil {
		return nil, err
	}
	return streamproc.ReadThrough(s.argv(s.decompressArgv, o), o.Nice, src)
}

// CompressCmd returns the compressor as a pipeline stage, or ok=false for the identity
// (none) codec, which contributes no stage. It lets the unified pipeline run compression
// through any executor (local or a remote client).
func CompressCmd(codec string, o Options) (cmd hostexec.Cmd, ok bool, err error) {
	return stageCmd(codec, func(s Spec) func(Options) []string { return s.compressArgv }, o)
}

// DecompressCmd returns the decompressor as a pipeline stage (the read-side peer of
// CompressCmd), or ok=false for none.
func DecompressCmd(codec string, o Options) (cmd hostexec.Cmd, ok bool, err error) {
	return stageCmd(codec, func(s Spec) func(Options) []string { return s.decompressArgv }, o)
}

func stageCmd(codec string, pick func(Spec) func(Options) []string, o Options) (hostexec.Cmd, bool, error) {
	s, err := spec(codec)
	if err != nil {
		return hostexec.Cmd{}, false, err
	}
	build := pick(s)
	if build == nil {
		return hostexec.Cmd{}, false, nil
	}
	argv := build(o)
	return hostexec.Cmd{Name: argv[0], Args: argv[1:], Nice: o.Nice}, true, nil
}

// argv applies an argv builder, returning nil for the none codec (no child) so
// streamproc runs the identity transform.
func (s Spec) argv(build func(Options) []string, o Options) []string {
	if build == nil {
		return nil
	}
	return build(o)
}
