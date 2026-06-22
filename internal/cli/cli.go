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

// loadConfigRO loads configuration for read-only commands. With -C it uses that
// directory directly (ignoring any config file). Otherwise it reads the config
// if present (surfacing parse/validation errors) or synthesizes a default
// catalog when no config file exists.
func loadConfigRO(cfgPath, catalogOverride string) (*config.Config, error) {
	if catalogOverride != "" {
		cfg := &config.Config{}
		applyCatalog(cfg, catalogOverride)
		return cfg, nil
	}
	if _, err := os.Stat(cfgPath); err != nil {
		cfg := &config.Config{}
		applyCatalog(cfg, "")
		return cfg, nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	applyCatalog(cfg, "")
	return cfg, nil
}

// applyCatalog applies a -C override (and a default when no media is configured)
// by defining a disk landing medium pointing at the directory.
func applyCatalog(cfg *config.Config, catalogOverride string) {
	if catalogOverride != "" {
		setLocalLanding(cfg, "cli", catalogOverride)
		return
	}
	if len(cfg.Media) == 0 {
		setLocalLanding(cfg, "default", DefaultCatalog)
	}
}

func setLocalLanding(cfg *config.Config, name, path string) {
	cfg.Media = map[string]config.Media{
		name: {Type: "disk", Params: map[string]string{"path": path}},
	}
	cfg.Landing = name
	cfg.Workdir = path
}

func newEngine(cfg *config.Config) (*engine.Engine, error) {
	return engine.New(cfg)
}

// logfStdout writes progress lines to stdout.
func logfStdout(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}
