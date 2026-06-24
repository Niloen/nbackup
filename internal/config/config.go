// Package config loads and validates NBackup configuration files. It also
// defines the configured domain entities — DLEs (backup sources), named media
// definitions, and dumptypes (named method+options bundles, in Amanda's sense).
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
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
	// Cycle is the dump cycle: the target — and hard maximum — time between fulls
	// for every DLE (e.g. "7d", the default). It is the one scheduling knob; how
	// runs are balanced within it is automatic (see package planner). A full never
	// ages past one cycle. Capacity-oriented retention (capacity, minimum_age)
	// lives per-medium, since each store has its own space and reuse cadence.
	Cycle string `yaml:"cycle"`

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

	// Encrypt configures the external encryptor archives are piped through, after
	// compression. It is the config-wide default; a dumptype may replace it wholesale
	// with its own `encrypt` block (see DumpType.Encrypt). Unset = no encryption.
	Encrypt EncryptConfig `yaml:"encrypt"`

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

	// Drill configures recovery drills (`nb drill`): the recoverability rehearsal
	// layered on `nb verify`. It mirrors the sync block so a cron line can be
	// `nb dump && nb sync && nb drill --unattended`.
	Drill DrillConfig `yaml:"drill"`

	// Notify configures unattended alerting: which channels fire on a run's
	// failure/success, and which receive `nb report --notify` digests. It is what
	// makes a cron-driven backup loud — a failed dump/sync/verify/drill reaches a
	// human. Secrets are referenced by environment variable, never stored here.
	Notify NotifyConfig `yaml:"notify"`

	// DumpTypes is a map of named method+option bundles (Amanda's dumptype).
	DumpTypes map[string]DumpType `yaml:"dumptypes"`

	Sources Sources `yaml:"sources"`
}

// DrillConfig is the `drill:` block: how often each DLE must be drilled, how many to
// drill per run, which copy to read, and how deeply. CLI flags override these.
type DrillConfig struct {
	Window     string `yaml:"window"`      // each DLE drilled within this window (default 30d)
	Sample     int    `yaml:"sample"`      // DLEs drilled per run (default 1)
	From       string `yaml:"from"`        // source medium to drill ("" = the landing medium)
	Tier       string `yaml:"tier"`        // checksum | structural | chain | stock (default structural)
	StockTools bool   `yaml:"stock_tools"` // drill via the documented stock one-liner (shorthand for tier: stock)
	Worm       bool   `yaml:"worm"`        // run the WORM/immutability probe
	Unattended bool   `yaml:"unattended"`  // cron mode: never prompt; skip swap-needing targets
}

// validDrillTiers is the accepted set for the drill tier token (kept here so config
// validation needs no dependency on package drill, which depends on no config).
var validDrillTiers = map[string]bool{"": true, "checksum": true, "structural": true, "chain": true, "stock": true}

// NotifyConfig is the `notify:` block: a set of named backends plus per-outcome
// routing. Failures must be loud, so when on_failure is omitted every configured
// backend fires on failure; success and digest notifications are opt-in. It mirrors
// the declarative shape of the sync/drill blocks.
type NotifyConfig struct {
	OnFailure []string `yaml:"on_failure"` // backends to notify on a failed run ("" = all backends)
	OnSuccess []string `yaml:"on_success"` // backends to notify on a successful run (default: none)
	Digest    []string `yaml:"digest"`     // backends for `nb report --notify` (default: none)

	Backends map[string]NotifyBackend `yaml:"backends"`
}

// NotifyBackend is one named notification channel. Connection settings are explicit
// fields (so `KnownFields(true)` rejects a stray key — including a literal
// `password:`/`token:`, which structurally enforces the env-reference rule below).
// Secrets are NEVER stored: they are named environment variables (password_env,
// url_env) resolved at send time, mirroring crypt's orchestrate-don't-hoard stance.
type NotifyBackend struct {
	Type string `yaml:"type"` // smtp | webhook (a registered notifier name)

	// smtp
	Host        string   `yaml:"host"`
	Port        int      `yaml:"port"`
	From        string   `yaml:"from"`
	To          []string `yaml:"to"`
	Username    string   `yaml:"username"`
	PasswordEnv string   `yaml:"password_env"` // env var holding the SMTP password (never the password itself)

	// webhook
	URL      string            `yaml:"url"`      // a non-secret endpoint; prefer url_env for anything secret
	URLEnv   string            `yaml:"url_env"`  // env var holding the webhook URL (Slack/Discord/PagerDuty secret)
	Headers  map[string]string `yaml:"headers"`  // optional extra HTTP headers
	Template string            `yaml:"template"` // optional payload field name for the message (default "text")
}

// validNotifyTypes is the accepted set for a backend's type (kept here so config
// validation needs no dependency on package notify, which depends on config).
var validNotifyTypes = map[string]bool{"smtp": true, "webhook": true}

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
// "path", s3 has "bucket"). Capacity and retention are per-medium because each
// store has its own space and reuse cadence (as in Amanda's per-storage
// retention).
type Media struct {
	Type       string            `yaml:"type"`
	Capacity   string            `yaml:"capacity"`    // space NBackup may use here, e.g. "20TB" ("" = unbounded)
	MinimumAge string            `yaml:"minimum_age"` // retention floor before a slot may be retired here (default: one cycle)
	Appendable *bool             `yaml:"appendable"`  // tape: pack many runs per tape (default) vs one run per tape
	Throughput string            `yaml:"throughput"`  // bandwidth cap to/from this medium, e.g. "50MB/s" ("" = uncapped); network politeness, the read/write peer of nice
	Cost       *CostConfig       `yaml:"cost"`        // optional pricing overrides; absent = inferred from type/url
	Params     map[string]string `yaml:",inline"`     // type-specific connection params (path, bucket, tapes, ...)
}

// CostConfig overrides a medium's inferred pricing. Every field is optional: an
// absent cost block (the common case) lets the medium price itself from its type and
// bucket URL scheme (s3:// = AWS, gs:// = GCS, azblob:// = Azure). A block is only for
// special cases — a different region's egress rate, or an S3-compatible provider's
// rates. Pointers distinguish an explicit value from an absent one, so an override of
// $0 is honored.
type CostConfig struct {
	Provider          string   `yaml:"provider"`             // base rate table to use (default: inferred from the url)
	StoragePerGBMonth *float64 `yaml:"storage_per_gb_month"` // recurring $/GiB-month
	EgressPerGB       *float64 `yaml:"egress_per_gb"`        // $/GiB transferred out
	GetPer1000        *float64 `yaml:"get_per_1000"`         // $ per 1000 read requests
}

// CostOptions flattens the medium's connection params (the bucket url, for scheme
// inference) and any cost-block overrides into the generic option map a media.Cost
// factory consumes — the dollar peer of ProfileOptions.
func (m Media) CostOptions() map[string]string {
	opts := map[string]string{}
	for k, v := range m.Params {
		opts[k] = v
	}
	if m.Cost == nil {
		return opts
	}
	if m.Cost.Provider != "" {
		opts["provider"] = m.Cost.Provider
	}
	putRate(opts, "storage_per_gb_month", m.Cost.StoragePerGBMonth)
	putRate(opts, "egress_per_gb", m.Cost.EgressPerGB)
	putRate(opts, "get_per_1000", m.Cost.GetPer1000)
	return opts
}

func putRate(opts map[string]string, key string, v *float64) {
	if v != nil {
		opts[key] = strconv.FormatFloat(*v, 'f', -1, 64)
	}
}

// IsAppendable reports whether a tape may accumulate many runs until full
// (Bacula-style, the default). When false, a tape holds a single run before it
// must be changed (Amanda-style). Address-identified media ignore it.
func (m Media) IsAppendable() bool { return m.Appendable == nil || *m.Appendable }

// CapacityBytes returns this medium's capacity in bytes, or 0 if unset (unbounded).
func (m Media) CapacityBytes() (int64, error) {
	if m.Capacity == "" {
		return 0, nil
	}
	return sizeutil.ParseBytes(m.Capacity)
}

// ThroughputBytes returns this medium's bandwidth cap in bytes per second, or 0
// if unset (uncapped). It caps both directions — a dump/sync to the medium and a
// restore/un-vault/drill from it — so the office uplink survives a business-hours
// backup. Concurrent dumpers to one medium share the single budget (Amanda's
// netusage), since a run writes a single landing medium.
func (m Media) ThroughputBytes() (int64, error) {
	if m.Throughput == "" {
		return 0, nil
	}
	return sizeutil.ParseRate(m.Throughput)
}

// ProfileOptions flattens the medium's capacity field and connection params into
// the generic option map a media.Profile factory consumes.
func (m Media) ProfileOptions() map[string]string {
	opts := map[string]string{}
	for k, v := range m.Params {
		opts[k] = v
	}
	opts["capacity"] = m.Capacity
	return opts
}

// MinAge returns this medium's explicitly configured retention floor, or 0 if
// unset. Callers that want the effective floor (defaulting to one cycle) should
// use Config.MinAgeFor instead.
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
	Method  string            `yaml:"method"`
	Encrypt *EncryptConfig    `yaml:"encrypt"` // nil = inherit the config-wide default; set = replace it wholesale (no field merge)
	Params  map[string]string `yaml:",inline"`
}

// EncryptConfig selects an encryption scheme and its key reference. The scheme is
// a compiled name (gpg|none); the key reference (recipient or passphrase file) is
// passed to the encryptor but never recorded — gpg owns the key material.
type EncryptConfig struct {
	Scheme         string `yaml:"scheme"`          // gpg | none (default none)
	Recipient      string `yaml:"recipient"`       // gpg public-key recipient (asymmetric)
	PassphraseFile string `yaml:"passphrase_file"` // gpg symmetric passphrase file
	Program        string `yaml:"program"`         // optional binary override (name or path)
}

// SchemeName returns the configured scheme, defaulting to "none".
func (e EncryptConfig) SchemeName() string {
	if e.Scheme == "" {
		return "none"
	}
	return e.Scheme
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
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config file at %s — copy nbackup.example.yaml to %s and edit it, or pass -c <path>", path, path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("config %s is empty — copy nbackup.example.yaml to %s and edit it", path, path)
	}
	var c Config
	// KnownFields rejects unknown keys so a typo in a safety-relevant field
	// (a misspelled `landing`, `cycle`, a nested compress/encrypt key) is a hard
	// error rather than a silently-ignored default. Type-specific medium and
	// dumptype params still flow through their inline maps, so connection keys
	// (path, url, bays, exclude, …) are unaffected.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks required fields and cross-references. A loaded config file must
// define any medium it names as `landing`; read-only commands with no config file
// build their default catalog without going through here (see cli.applyCatalog).
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
	if err := c.landingDefined(); err != nil {
		return err
	}
	if c.Cycle != "" {
		d, err := sizeutil.ParseDuration(c.Cycle)
		if err != nil {
			return fmt.Errorf("cycle: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("cycle must be positive (e.g. 7d); got %q", c.Cycle)
		}
	}
	for name, m := range c.Media {
		if _, err := m.CapacityBytes(); err != nil {
			return fmt.Errorf("media %s: capacity: %w", name, err)
		}
		if _, err := m.MinAge(); err != nil {
			return fmt.Errorf("media %s: minimum_age: %w", name, err)
		}
		if _, err := m.ThroughputBytes(); err != nil {
			return fmt.Errorf("media %s: throughput: %w", name, err)
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
	if err := c.validateDrill(); err != nil {
		return err
	}
	if err := c.validateNotify(); err != nil {
		return err
	}
	return nil
}

// validateNotify checks the optional `notify:` block: every backend has a known
// type and its required connection fields, and every routing entry names a defined
// backend. The env-reference secrets rule is enforced structurally by
// KnownFields(true) (a literal password/token key is an unknown field).
func (c *Config) validateNotify() error {
	n := c.Notify
	if len(n.Backends) == 0 {
		if len(n.OnFailure)+len(n.OnSuccess)+len(n.Digest) > 0 {
			return fmt.Errorf("notify: routing names a backend but no backends are defined")
		}
		return nil
	}
	for name, b := range n.Backends {
		if !validNotifyTypes[b.Type] {
			return fmt.Errorf("notify: backend %q: unknown type %q (known: smtp, webhook)", name, b.Type)
		}
		switch b.Type {
		case "smtp":
			if b.Host == "" || b.From == "" || len(b.To) == 0 {
				return fmt.Errorf("notify: smtp backend %q requires host, from, and at least one recipient (to)", name)
			}
		case "webhook":
			if b.URLEnv == "" && b.URL == "" {
				return fmt.Errorf("notify: webhook backend %q requires url_env (preferred for secret endpoints) or url", name)
			}
		}
	}
	for _, group := range [][]string{n.OnFailure, n.OnSuccess, n.Digest} {
		for _, name := range group {
			if _, ok := n.Backends[name]; !ok {
				return fmt.Errorf("notify: routing references undefined backend %q", name)
			}
		}
	}
	return nil
}

// validateDrill checks the optional `drill:` block.
func (c *Config) validateDrill() error {
	d := c.Drill
	if d.Window != "" {
		if _, err := sizeutil.ParseDuration(d.Window); err != nil {
			return fmt.Errorf("drill: window: %w", err)
		}
	}
	if d.Sample < 0 {
		return fmt.Errorf("drill: sample must not be negative")
	}
	if !validDrillTiers[d.Tier] {
		return fmt.Errorf("drill: unknown tier %q (known: checksum, structural, chain, stock)", d.Tier)
	}
	if d.From != "" && len(c.Media) > 0 {
		if _, ok := c.Media[d.From]; !ok {
			return fmt.Errorf("drill: source %q is not a defined medium", d.From)
		}
	}
	return nil
}

// DefaultDrillWindow is the assumed drill coverage window when `drill.window` is
// unset: every DLE should be drilled at least this often.
const DefaultDrillWindow = 30 * 24 * time.Hour

// DefaultDrillSample is the number of DLEs drilled per run when `drill.sample` is
// unset — a small, risk-biased rotation so routine drills stay cheap.
const DefaultDrillSample = 1

// DrillWindow returns the drill coverage window (default DefaultDrillWindow).
func (c *Config) DrillWindow() time.Duration {
	if c.Drill.Window == "" {
		return DefaultDrillWindow
	}
	if d, err := sizeutil.ParseDuration(c.Drill.Window); err == nil && d > 0 {
		return d
	}
	return DefaultDrillWindow
}

// DrillSample returns the per-run DLE sample size (default DefaultDrillSample).
func (c *Config) DrillSample() int {
	if c.Drill.Sample > 0 {
		return c.Drill.Sample
	}
	return DefaultDrillSample
}

// DrillTierName returns the configured drill tier token, applying the stock_tools
// shorthand and defaulting to "structural" (the no-write tier fit for routine drills).
func (c *Config) DrillTierName() string {
	if c.Drill.StockTools {
		return "stock"
	}
	if c.Drill.Tier != "" {
		return c.Drill.Tier
	}
	return "structural"
}

// DLEs returns the configured backup sources.
func (c *Config) DLEs() []DLE { return c.Sources }

// landingDefined reports an error if a non-empty `landing` names a medium that is
// not defined. This holds even when the media map is empty: a config that sets
// `landing:` but omits its `media:` block is a misconfig, not a cue to silently
// synthesize a default medium (which would discard the requested landing and write
// to ./nbackup-catalog). An empty landing is not an error here — LandingName resolves
// it via the sole-medium fallback. Shared by Validate and LandingName so the rule
// lives once.
func (c *Config) landingDefined() error {
	if c.Landing != "" {
		if _, ok := c.Media[c.Landing]; !ok {
			return fmt.Errorf("landing %q is not a defined medium", c.Landing)
		}
	}
	return nil
}

// LandingName resolves the name of the medium used for landing: the configured
// `landing`, or the sole medium when exactly one is defined.
func (c *Config) LandingName() (string, error) {
	if c.Landing != "" {
		if err := c.landingDefined(); err != nil {
			return "", err
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

// EncryptionFor returns the encryption settings for a dumptype: its own `encrypt`
// block if it sets one, otherwise the config-wide default. The override replaces
// the default wholesale — fields are not merged.
func (c *Config) EncryptionFor(dtName string) EncryptConfig {
	if dt, ok := c.DumpTypes[dtName]; ok && dt.Encrypt != nil {
		return *dt.Encrypt
	}
	return c.Encrypt
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

// DefaultCycle is the dump cycle assumed when `cycle` is unset.
const DefaultCycle = 7 * 24 * time.Hour

// CycleDuration returns the dump cycle as a duration (default DefaultCycle).
func (c *Config) CycleDuration() time.Duration {
	if c.Cycle == "" {
		return DefaultCycle
	}
	d, err := sizeutil.ParseDuration(c.Cycle)
	if err != nil || d <= 0 {
		return DefaultCycle
	}
	return d
}

// CycleDays returns the dump cycle in whole days (default 7).
func (c *Config) CycleDays() int {
	days := int(c.CycleDuration().Hours() / 24)
	if days < 1 {
		days = 1
	}
	return days
}

// MinAgeFor returns a medium's effective retention floor: its configured
// minimum_age, or the dump cycle when unset. Defaulting to one cycle keeps the
// "yesterday must not overwrite last month" safety without a knob — a slot stays
// retainable for at least the window in which it is still a recovery base.
func (c *Config) MinAgeFor(m Media) time.Duration {
	if age, err := m.MinAge(); err == nil && age > 0 {
		return age
	}
	return c.CycleDuration()
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
