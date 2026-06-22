// Package config loads and validates NBackup configuration files. It also
// defines the configured domain entities — DLEs (backup sources), named media
// definitions, and dumptypes (named method+options bundles, in Amanda's sense).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
	"gopkg.in/yaml.v3"
)

// DefaultDumpType is used by DLEs that do not name one.
const DefaultDumpType = "default"

// DefaultMethod is the dump method assumed when a dumptype omits one.
const DefaultMethod = "gnutar"

// Config is the top-level NBackup configuration.
type Config struct {
	// Cycle holds cross-cutting retention safety. Capacity-oriented retention
	// (budget, minimum_age) lives per-medium, not here.
	Cycle struct {
		RequireVerifiedSuccessor bool `yaml:"require_verified_successor"`
	} `yaml:"cycle"`

	// Landing names the media definition where slots are created.
	Landing string `yaml:"landing"`

	Planner struct {
		FullIntervalDays int `yaml:"full_interval_days"`
	} `yaml:"planner"`

	// GnuTarPath is a global default GNU tar binary for the gnutar method.
	GnuTarPath string `yaml:"gnutar_path"`

	// Workdir holds local operational state (slot cache + snapshot library).
	// Defaults to the landing medium's path when that medium is local-disk.
	Workdir string `yaml:"workdir"`

	// Media is a map of named storage definitions.
	Media map[string]Media `yaml:"media"`

	// DumpTypes is a map of named method+option bundles (Amanda's dumptype).
	DumpTypes map[string]DumpType `yaml:"dumptypes"`

	Sources []DLE `yaml:"sources"`
}

// Media is one named storage definition: a type, capacity/retention policy for
// this medium, and type-specific connection parameters (e.g. local-disk has
// "path", s3 has "bucket"). Budget and retention are per-medium because each
// store has its own capacity and reuse cycle (as in Amanda's per-storage
// retention).
type Media struct {
	Type       string            `yaml:"type"`
	Budget     string            `yaml:"budget"`      // capacity ceiling, e.g. "20TB" ("" = unbounded)
	MinimumAge string            `yaml:"minimum_age"` // cycle age before a slot may be retired here
	Params     map[string]string `yaml:",inline"`     // type-specific connection params (path, bucket, tapes, ...)
}

// BudgetBytes returns this medium's capacity ceiling in bytes, or 0 if unset.
func (m Media) BudgetBytes() (int64, error) {
	if m.Budget == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(m.Budget)
}

// ProfileOptions flattens the medium's capacity field and connection params into
// the generic option map a media.Profile factory consumes.
func (m Media) ProfileOptions() map[string]string {
	opts := map[string]string{}
	for k, v := range m.Params {
		opts[k] = v
	}
	opts["budget"] = m.Budget
	return opts
}

// MinAge returns this medium's cycle minimum age, or 0 if unset.
func (m Media) MinAge() (time.Duration, error) {
	if m.MinimumAge == "" {
		return 0, nil
	}
	return sizeutil.ParseDuration(m.MinimumAge)
}

// DumpType bundles a dump method with its options, referenced by DLEs.
type DumpType struct {
	Method string            `yaml:"method"`
	Params map[string]string `yaml:",inline"`
}

// DLE is a backup source: a path on a host, dumped per a named dumptype.
type DLE struct {
	Host     string `yaml:"host"`
	Path     string `yaml:"path"`
	DumpType string `yaml:"dumptype"`
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Name returns a stable, filesystem-safe identifier for the DLE, e.g.
// host "app01" + path "/home" -> "app01-home".
func (d DLE) Name() string {
	p := strings.Trim(d.Path, "/")
	p = strings.ReplaceAll(p, "/", "-")
	if p == "" {
		p = "root"
	}
	return slugStrip.ReplaceAllString(d.Host+"-"+p, "_")
}

// DumpTypeName returns the DLE's dumptype, defaulting to "default".
func (d DLE) DumpTypeName() string {
	if d.DumpType != "" {
		return d.DumpType
	}
	return DefaultDumpType
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and cross-references. It is lenient about a
// missing media/landing so read-only commands can synthesize defaults.
func (c *Config) Validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("config has no sources")
	}
	for i, s := range c.Sources {
		if s.Host == "" || s.Path == "" {
			return fmt.Errorf("source %d: host and path are required", i)
		}
		dt := s.DumpTypeName()
		if dt != DefaultDumpType {
			if _, ok := c.DumpTypes[dt]; !ok {
				return fmt.Errorf("source %s: unknown dumptype %q", s.Name(), dt)
			}
		}
	}
	if c.Landing != "" && len(c.Media) > 0 {
		if _, ok := c.Media[c.Landing]; !ok {
			return fmt.Errorf("landing %q is not a defined medium", c.Landing)
		}
	}
	for name, m := range c.Media {
		if _, err := m.BudgetBytes(); err != nil {
			return fmt.Errorf("media %s: budget: %w", name, err)
		}
		if _, err := m.MinAge(); err != nil {
			return fmt.Errorf("media %s: minimum_age: %w", name, err)
		}
	}
	return nil
}

// DLEs returns the configured backup sources.
func (c *Config) DLEs() []DLE { return c.Sources }

// LandingMedia resolves the media definition used for landing. If no landing is
// named but exactly one medium is defined, that one is used.
func (c *Config) LandingMedia() (Media, error) {
	if c.Landing == "" {
		if len(c.Media) == 1 {
			for _, m := range c.Media {
				return m, nil
			}
		}
		return Media{}, fmt.Errorf("no landing medium selected (set `landing:` to a media name)")
	}
	m, ok := c.Media[c.Landing]
	if !ok {
		return Media{}, fmt.Errorf("landing %q is not a defined medium", c.Landing)
	}
	return m, nil
}

// ResolveDumpType returns the named dumptype, applying the default method when
// unset and falling back to a gnutar default for unknown names.
func (c *Config) ResolveDumpType(name string) DumpType {
	dt, ok := c.DumpTypes[name]
	if !ok {
		return DumpType{Method: DefaultMethod}
	}
	if dt.Method == "" {
		dt.Method = DefaultMethod
	}
	return dt
}

// FullIntervalDays returns the target days between full backups for a DLE.
func (c *Config) FullIntervalDays() int {
	if c.Planner.FullIntervalDays > 0 {
		return c.Planner.FullIntervalDays
	}
	return 7
}

// WorkdirPath returns the local operational-state directory, defaulting to the
// landing medium's path when that medium is local-disk.
func (c *Config) WorkdirPath() string {
	if c.Workdir != "" {
		return c.Workdir
	}
	if m, err := c.LandingMedia(); err == nil && m.Type == "local-disk" {
		return m.Params["path"]
	}
	return ""
}
