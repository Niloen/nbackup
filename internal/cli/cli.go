// Package cli holds helpers shared by the nb* command-line tools. Commands are
// thin wrappers that build an engine.Engine from configuration and render its
// results.
package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
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

// ParseDate parses a YYYY-MM-DD date, or returns today (UTC) when empty.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC().Truncate(24 * time.Hour), nil
	}
	return time.Parse("2006-01-02", s)
}

// loadConfig loads configuration for commands that need full config (plan/dump),
// applying a catalog override and a default catalog path.
func loadConfig(cfgPath, catalogOverride string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	applyCatalog(cfg, catalogOverride)
	return cfg, nil
}

// loadConfigRO loads configuration for read-only commands, synthesizing a
// minimal config when none exists so a bare -C still works.
func loadConfigRO(cfgPath, catalogOverride string) *config.Config {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		cfg = &config.Config{}
		cfg.Landing.Media = "local-disk"
	}
	applyCatalog(cfg, catalogOverride)
	return cfg
}

func applyCatalog(cfg *config.Config, catalogOverride string) {
	if catalogOverride != "" {
		cfg.Media.LocalDisk.Path = catalogOverride
		cfg.Workdir = catalogOverride
	}
	if cfg.Media.LocalDisk.Path == "" {
		cfg.Media.LocalDisk.Path = DefaultCatalog
	}
	if cfg.Landing.Media == "" {
		cfg.Landing.Media = "local-disk"
	}
}

func newEngine(cfg *config.Config) (*engine.Engine, error) {
	return engine.New(cfg)
}

// logfStdout writes progress lines to stdout.
func logfStdout(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}
