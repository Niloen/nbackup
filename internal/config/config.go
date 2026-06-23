// Package config loads and validates NBackup configuration files. It also
// defines the configured domain entities — DLEs (backup sources), named media
// definitions, and dumptypes (named method+options bundles, in Amanda's sense).
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
	"gopkg.in/yaml.v3"
)

// DefaultDumpType is used by DLEs that do not name one.
const DefaultDumpType = "default"

// DefaultCodec is the compression codec assumed when compress.codec is unset.
const DefaultCodec = "zstd"

// DefaultMethod is the dump method assumed when a dumptype omits one.
const DefaultMethod = "gnutar"

// DefaultWorkdir is where the catalog cache and snapshot library live when
// `workdir` is unset. It is deliberately independent of any storage medium: the
// catalog is a cache over the whole pool, not a thing owned by one medium.
const DefaultWorkdir = "nbackup-catalog"

// Config is the top-level NBackup configuration.
type Config struct {
	// Cycle holds cross-cutting retention safety. Capacity-oriented retention
	// (budget, minimum_age) lives per-medium, not here.
	// Cycle is the dump cycle and its retention safety: how often each DLE gets
	// a full, how runs are balanced, and the cross-cutting recovery guarantee.
	Cycle struct {
		// Length is the dump cycle: target time between fulls per DLE (e.g. "7d").
		Length string `yaml:"length"`
		// RequireVerifiedSuccessor: never retire the last verified recovery path.
		RequireVerifiedSuccessor bool `yaml:"require_verified_successor"`
		// Promote enables pulling future fulls forward to fill light runs.
		// Off by default so balancing never spends extra storage.
		Promote bool `yaml:"promote"`
		// PromoteHeadroom caps promotion at this fraction of capacity (default 0.8).
		PromoteHeadroom float64 `yaml:"promote_headroom"`
	} `yaml:"cycle"`

	// Landing names the media definition where slots are created.
	Landing string `yaml:"landing"`

	// GnuTarPath is a global default GNU tar binary for the gnutar method.
	GnuTarPath string `yaml:"gnutar_path"`

	// Workdir holds the catalog's local state (slot cache + snapshot library),
	// independent of any storage medium. Defaults to DefaultWorkdir.
	Workdir string `yaml:"workdir"`

	// Compress configures the external compressor archives are piped through.
	Compress struct {
		Codec   string `yaml:"codec"`   // zstd|gzip|none (default zstd)
		Level   int    `yaml:"level"`   // codec level; 0 = codec default
		Threads int    `yaml:"threads"` // worker threads where supported; 0 = codec default
		Program string `yaml:"program"` // optional binary override (name or path)
	} `yaml:"compress"`

	// Nice runs orchestrated child processes under `nice -n Nice` for CPU
	// politeness; 0 = no nice.
	Nice int `yaml:"nice"`

	// AutoLabel lets a dump label a blank tape automatically instead of requiring
	// an explicit `nb label`. Off by default: explicit labeling is what makes the
	// overwrite guard meaningful. It never clobbers foreign or non-blank media.
	AutoLabel bool `yaml:"auto_label"`

	// Parallelism bounds concurrent work within a run.
	Parallelism struct {
		Dumpers int `yaml:"dumpers"` // concurrent DLE dumps per run (default 1)
	} `yaml:"parallelism"`

	// Media is a map of named storage definitions.
	Media map[string]Media `yaml:"media"`

	// Sync declares replication rules: each mirrors the landing medium's sealed
	// slots onto a target medium (Amanda's vaulting). `nb sync` with no --to runs
	// every rule; `nb sync --to X` is the ad-hoc form and needs no rule.
	Sync []SyncRule `yaml:"sync"`

	// DumpTypes is a map of named method+option bundles (Amanda's dumptype).
	DumpTypes map[string]DumpType `yaml:"dumptypes"`

	Sources Sources `yaml:"sources"`
}

// Sources is the disklist. In config it is written grouped by dumptype, then
// host, then a list of paths:
//
//	sources:
//	  default:
//	    app01: [/home, /etc]
//	  no-logs:
//	    db01: [/var/lib/postgresql]
//
// It flattens to a sorted list of DLEs. Per-DLE behavior lives in the named
// dumptype, not the entry (as in Amanda).
type Sources []DLE

// UnmarshalYAML decodes the grouped form into a flat, sorted []DLE.
func (s *Sources) UnmarshalYAML(node *yaml.Node) error {
	var raw map[string]map[string][]string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("sources must be a mapping of dumptype -> {host: [paths]}: %w", err)
	}
	var dles []DLE
	for dumptype, hosts := range raw {
		for host, paths := range hosts {
			for _, path := range paths {
				dles = append(dles, DLE{Host: host, Path: path, DumpType: dumptype})
			}
		}
	}
	sort.Slice(dles, func(i, j int) bool {
		if dles[i].Host != dles[j].Host {
			return dles[i].Host < dles[j].Host
		}
		if dles[i].Path != dles[j].Path {
			return dles[i].Path < dles[j].Path
		}
		return dles[i].DumpType < dles[j].DumpType
	})
	*s = dles
	return nil
}

// Media is one named storage definition: a type, capacity/retention policy for
// this medium, and type-specific connection parameters (e.g. disk has
// "path", s3 has "bucket"). Budget and retention are per-medium because each
// store has its own capacity and reuse cycle (as in Amanda's per-storage
// retention).
type Media struct {
	Type       string            `yaml:"type"`
	Budget     string            `yaml:"budget"`      // capacity ceiling, e.g. "20TB" ("" = unbounded)
	MinimumAge string            `yaml:"minimum_age"` // cycle age before a slot may be retired here
	Appendable *bool             `yaml:"appendable"`  // tape: pack many runs per tape (default) vs one run per tape
	Params     map[string]string `yaml:",inline"`     // type-specific connection params (path, bucket, tapes, ...)
}

// IsAppendable reports whether a tape may accumulate many runs until full
// (Bacula-style, the default). When false, a tape holds a single run before it
// must be changed (Amanda-style). Address-identified media ignore it.
func (m Media) IsAppendable() bool { return m.Appendable == nil || *m.Appendable }

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

// SyncRule mirrors one medium's slots onto another. Selection bounds keep an
// expensive target (tape, object store) to a recent window; an unbounded rule
// replicates everything. The source defaults to the landing medium (the same
// medium `nb copy` streams from) but may be any other medium via `from`.
type SyncRule struct {
	To   string `yaml:"to"`   // target medium name (required; must differ from the source)
	From string `yaml:"from"` // source medium name ("" = the landing medium)
	Last int    `yaml:"last"` // copy only the N most recent slots (0 = all)
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
	if c.Cycle.Length != "" {
		if _, err := sizeutil.ParseDuration(c.Cycle.Length); err != nil {
			return fmt.Errorf("cycle.length: %w", err)
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
	for i, r := range c.Sync {
		if r.To == "" {
			return fmt.Errorf("sync rule %d: `to` is required", i)
		}
		if len(c.Media) > 0 {
			if _, ok := c.Media[r.To]; !ok {
				return fmt.Errorf("sync rule %d: target %q is not a defined medium", i, r.To)
			}
			if r.From != "" {
				if _, ok := c.Media[r.From]; !ok {
					return fmt.Errorf("sync rule %d: source %q is not a defined medium", i, r.From)
				}
			}
		}
		from := r.From
		if from == "" {
			from = c.Landing
		}
		if from == r.To {
			return fmt.Errorf("sync rule %d: source and target are the same medium %q", i, r.To)
		}
		if r.Last < 0 {
			return fmt.Errorf("sync rule %d: `last` must not be negative", i)
		}
	}
	return nil
}

// DLEs returns the configured backup sources.
func (c *Config) DLEs() []DLE { return c.Sources }

// LandingName resolves the name of the medium used for landing: the configured
// `landing`, or the sole medium when exactly one is defined.
func (c *Config) LandingName() (string, error) {
	if c.Landing != "" {
		if _, ok := c.Media[c.Landing]; !ok && len(c.Media) > 0 {
			return "", fmt.Errorf("landing %q is not a defined medium", c.Landing)
		}
		return c.Landing, nil
	}
	if len(c.Media) == 1 {
		for name := range c.Media {
			return name, nil
		}
	}
	return "", fmt.Errorf("no landing medium selected (set `landing:` to a media name)")
}

// LandingMedia resolves the media definition used for landing. If no landing is
// named but exactly one medium is defined, that one is used.
func (c *Config) LandingMedia() (Media, error) {
	name, err := c.LandingName()
	if err != nil {
		return Media{}, err
	}
	return c.Media[name], nil
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

// CompressCodec returns the configured codec, defaulting to zstd.
func (c *Config) CompressCodec() string {
	if c.Compress.Codec != "" {
		return c.Compress.Codec
	}
	return DefaultCodec
}

// Dumpers returns the number of concurrent DLE dumps per run (default 1).
func (c *Config) Dumpers() int {
	if c.Parallelism.Dumpers > 1 {
		return c.Parallelism.Dumpers
	}
	return 1
}

// FullIntervalDays returns the dump cycle in whole days (default 7).
func (c *Config) FullIntervalDays() int {
	if c.Cycle.Length == "" {
		return 7
	}
	d, err := sizeutil.ParseDuration(c.Cycle.Length)
	if err != nil || d <= 0 {
		return 7
	}
	days := int(d.Hours() / 24)
	if days < 1 {
		days = 1
	}
	return days
}

// WorkdirPath returns the catalog's own operational-state directory (slot cache +
// snapshot library), independent of any storage medium. It defaults to
// DefaultWorkdir when `workdir` is unset.
func (c *Config) WorkdirPath() string {
	if c.Workdir != "" {
		return c.Workdir
	}
	return DefaultWorkdir
}
