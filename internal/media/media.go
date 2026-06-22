// Package media is NBackup's storage abstraction, analogous to Amanda's Device
// API. A Store is a landing medium where immutable slots are authored and read
// (local-disk today; S3 later). Implementations register themselves with this
// package, so selecting a medium is a registry lookup rather than a conditional
// in the core. Secondary copies (to tape, another bucket, ...) will be modeled
// later as copies between Stores, not as a separate vault abstraction.
package media

import (
	"fmt"
	"io"
	"sort"
)

// Options carries medium-specific configuration to a factory as generic
// key/value parameters (e.g. "path" for local-disk, "bucket" for s3). Keeping
// it generic means adding a medium type requires no change here.
type Options map[string]string

// Get returns the value for a parameter key, or "".
func (o Options) Get(key string) string { return o[key] }

// Store is a landing medium: slots are created, read, listed, and removed as
// sets of named objects. The slot format is identical across stores.
type Store interface {
	Name() string
	Create(slotID, name string) (io.WriteCloser, error)
	Open(slotID, name string) (io.ReadCloser, error)
	ListSlots() ([]string, error)
	Remove(slotID string) error
}

// StoreFactory constructs a Store from options.
type StoreFactory func(Options) (Store, error)

var storeFactories = map[string]StoreFactory{}

// RegisterStore registers a Store implementation under a medium type name.
func RegisterStore(typ string, f StoreFactory) { storeFactories[typ] = f }

// OpenStore constructs the Store registered for the given medium type.
func OpenStore(typ string, opts Options) (Store, error) {
	f, ok := storeFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown landing medium %q (known: %v)", typ, StoreTypes())
	}
	return f(opts)
}

// StoreTypes lists registered Store medium types.
func StoreTypes() []string { return keys(storeFactories) }

// ErrNotImplemented is returned by registered-but-incomplete media.
var ErrNotImplemented = fmt.Errorf("not implemented in this version")

func keys(m map[string]StoreFactory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
