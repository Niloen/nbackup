package archiver

import (
	"fmt"
	"sort"

	"github.com/Niloen/nbackup/internal/programs"
)

// Factory constructs an Archiver from options, the executor (host) its programs run on,
// and stateRoot — this archiver's private directory for incremental state, under which it
// keys by DLE and level. The caller derives stateRoot from the host's shared state_dir,
// namespaced by archiver type (e.g. <state_dir>/gnutar), so archivers sharing a host
// don't collide; the archiver owns everything beneath it. The executor makes remote
// execution transparent: an archiver runs its tools through it without knowing whether the
// host is local or a client over SSH, and stateRoot resolves on that same host so the
// incremental state lives where the data is read. It is a parameter, not an Option,
// because the location is the host's to decide, not a format property.
type Factory func(Options, programs.Executor, string) (Archiver, error)

var factories = map[string]Factory{}

// knownOptions records the option keys each archiver type accepts, so config load can
// reject a typo'd option (e.g. `one-file-sytem`) rather than silently dropping it — an
// inline option map bypasses YAML's KnownFields check, so the registry is the one place
// that knows a type's real option set.
var knownOptions = map[string][]string{}

// Register registers an Archiver implementation under a type name, declaring the option
// keys it accepts (used by config validation to reject unknown options).
func Register(name string, opts []string, f Factory) {
	factories[name] = f
	knownOptions[name] = opts
}

// KnownOptions returns the option keys the named archiver type accepts, and whether the
// type is registered.
func KnownOptions(name string) ([]string, bool) {
	opts, ok := knownOptions[name]
	return opts, ok
}

// Open constructs the Archiver registered under the type name, running its programs
// through ex (local or a remote client) and keeping incremental state under stateRoot.
func Open(name string, opts Options, ex programs.Executor, stateRoot string) (Archiver, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown archiver %q (known: %v)", name, Names())
	}
	return f(opts, ex, stateRoot)
}

// Names lists registered archiver type names.
func Names() []string {
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
