// Package cli holds helpers shared by the nb* command-line tools.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/slot"
)

// DefaultConfigPath is used when -c is not given.
const DefaultConfigPath = "nbackup.yaml"

// DefaultCatalog is used when neither -C nor config provides a catalog path.
const DefaultCatalog = "nbackup-catalog"

// Fatalf prints to stderr and exits non-zero.
func Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// ResolveCatalog determines the catalog (slot store) directory. Precedence:
// explicit flag, then config media.local-disk.path, then the default.
func ResolveCatalog(flagCatalog string, cfg *config.Config) string {
	if flagCatalog != "" {
		return flagCatalog
	}
	if cfg != nil && cfg.Media.LocalDisk.Path != "" {
		return cfg.Media.LocalDisk.Path
	}
	return DefaultCatalog
}

// ParseDate parses a YYYY-MM-DD date, or returns today (UTC) when empty.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC().Truncate(24 * time.Hour), nil
	}
	return time.Parse("2006-01-02", s)
}

// CatalogBytes returns the total compressed size recorded across all slots.
func CatalogBytes(catalog string) (int64, error) {
	slots, err := slot.List(catalog)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, s := range slots {
		total += s.TotalBytes
	}
	return total, nil
}

// SlotDir returns the directory for a slot ID under the catalog.
func SlotDir(catalog, slotID string) string {
	return filepath.Join(catalog, slotID)
}
