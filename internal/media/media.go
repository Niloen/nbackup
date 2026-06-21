// Package media is NBackup's storage abstraction, analogous to Amanda's Device
// API. A Store is a landing medium where immutable slots are authored and read
// (local-disk today; S3 later). A Vault is a secondary copy target that holds a
// slot serialized as a stream (tape/glacier later). Implementations register
// themselves with this package, so selecting a medium is a registry lookup
// rather than a conditional in the core.
package media

import (
	"fmt"
	"io"
	"sort"
)

// Object is metadata about a single file within a slot.
type Object struct {
	Name string
	Size int64
}

// Options carries medium-specific configuration to a factory. Each
// implementation reads the fields it understands.
type Options struct {
	Path   string // local-disk: root directory
	Bucket string // s3: bucket name
	Device string // tape: device path
}

// Store is a landing medium: slots are created, read, listed, and removed as
// sets of named objects. The slot format is identical across stores.
type Store interface {
	Name() string
	Create(slotID, name string) (io.WriteCloser, error)
	Open(slotID, name string) (io.ReadCloser, error)
	Stat(slotID, name string) (Object, error)
	List(slotID string) ([]Object, error)
	ListSlots() ([]string, error)
	Remove(slotID string) error
}

// Vault is a secondary copy target: a sealed slot serialized as one stream.
// (Tape spanning across volumes is the implementation's concern.)
type Vault interface {
	Name() string
	Put(slotID string, r io.Reader) error
	Get(slotID string) (io.ReadCloser, error)
	ListSlots() ([]string, error)
}

// StoreFactory constructs a Store from options.
type StoreFactory func(Options) (Store, error)

// VaultFactory constructs a Vault from options.
type VaultFactory func(Options) (Vault, error)

var (
	storeFactories = map[string]StoreFactory{}
	vaultFactories = map[string]VaultFactory{}
)

// RegisterStore registers a Store implementation under a medium type name.
func RegisterStore(typ string, f StoreFactory) { storeFactories[typ] = f }

// RegisterVault registers a Vault implementation under a medium type name.
func RegisterVault(typ string, f VaultFactory) { vaultFactories[typ] = f }

// OpenStore constructs the Store registered for the given medium type.
func OpenStore(typ string, opts Options) (Store, error) {
	f, ok := storeFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown landing medium %q (known: %v)", typ, StoreTypes())
	}
	return f(opts)
}

// OpenVault constructs the Vault registered for the given medium type.
func OpenVault(typ string, opts Options) (Vault, error) {
	f, ok := vaultFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown vault medium %q (known: %v)", typ, VaultTypes())
	}
	return f(opts)
}

// StoreTypes lists registered Store medium types.
func StoreTypes() []string { return keys(storeFactories) }

// VaultTypes lists registered Vault medium types.
func VaultTypes() []string { return vaultKeys(vaultFactories) }

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

func vaultKeys(m map[string]VaultFactory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
