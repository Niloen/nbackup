// Package config loads and validates NBackup configuration files. It also
// defines the configured domain entities — DLEs (backup sources), named media
// definitions, named archiver definitions, and dumptypes
// (an archiver reference plus per-DLE policy).
package config

import (
	"fmt"
	"sort"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
)

// Config is the top-level NBackup configuration. Fields carry `omitempty` so a
// programmatically built config (`nb init`) marshals to only what was chosen —
// unset knobs stay absent and keep their documented defaults; loading is
// unaffected.
type Config struct {
	// Cycle is the dump cycle: the target — and hard maximum — time between fulls
	// for every DLE (e.g. "7d", the default). It is the one scheduling knob; how
	// runs are balanced within it is automatic (see package planner). A full never
	// ages past one cycle. Capacity-oriented retention (capacity, minimum_age)
	// lives per-medium, since each store has its own space and reuse cadence.
	Cycle string `yaml:"cycle,omitempty"`

	// BumpPct is the minimum saving — as a percentage of the full-dump size — an
	// incremental must show before it climbs to the next level. A higher level
	// captures only what changed since the level below,
	// so it is taken only when that is a real saving; otherwise the DLE stays at its
	// current level, re-dumping everything since the lower one (which also keeps
	// consecutive incrementals overlapping, so losing one does not break the chain).
	// Default DefaultBumpPercent. See package planner.
	BumpPct float64 `yaml:"bump_percent,omitempty"`

	// Landing names the media definition(s) where runs are created: a single name, or
	// a list to write every archive to several media (the first entry is the primary —
	// see MediumList).
	Landing MediumList `yaml:"landing,omitempty"`

	// Workdir holds the catalog's local cache, independent of any storage medium.
	// Defaults to DefaultWorkdir. (An archiver's incremental state lives under the host's
	// state_dir — a dedicated location beside the workdir, not beneath it; see StateDir.)
	Workdir string `yaml:"workdir,omitempty"`

	// StateDir is the fleet-wide default root for archivers' incremental-state libraries
	// (gnutar's .snar files, a future archiver's dump database, …). It is a host-level,
	// archiver-agnostic location: every archiver on a host shares it and namespaces its
	// own state beneath it. A per-host `hosts.<h>.state_dir` overrides it; unset, it
	// falls back to DefaultStateDir. The location is the host's, not a format property,
	// so it is not an archiver option.
	StateDir string `yaml:"state_dir,omitempty"`

	// SecretsDir is the pool-side root for credentials a medium's `nb login` mints (the
	// gdrive OAuth token). Like StateDir it is a dedicated location BESIDE the workdir, not
	// beneath it: the workdir is a disposable cache that rebuild wipes, whereas a login
	// token is precious, non-rebuildable state — only a fresh consent brings it back — so
	// nesting it under the workdir would invite its loss. Defaults to DefaultSecretsDir.
	SecretsDir string `yaml:"secrets_dir,omitempty"`

	// PartSize is the config-wide default ATOM size for encrypted (atomic-shape)
	// archives: each part of such an archive is one complete encrypted message of at
	// most this many compressed bytes, cut at dump time and carried unchanged by every
	// copy ("an atomic archive brings its own part size; a sliced archive takes the
	// medium's" — a medium's part_size keeps its slice meaning and is never consulted
	// for atoms). A dumptype may override it (DumpType.PartSize) — the selective-
	// restore tuning lever: smaller atoms, finer encrypted restore granularity and
	// cheaper key-proving drills, more objects. Default DefaultAtomSize.
	PartSize string `yaml:"part_size,omitempty"`

	// FrameSize is the raw-stream interval at which a framed archive's encode pipeline
	// restarts — a decode-restart point every this-many bytes of tar stream, recorded in
	// the archive's frame table (see docs/design/archive-shapes.md). It is an advanced
	// internal knob: frames never exist as files (deliberately outside the "part"
	// vocabulary); the default (DefaultFrameSize) is right for almost everyone, and
	// smaller frames only trade a sliver of compression ratio for finer ranged reads.
	FrameSize string `yaml:"frame_size,omitempty"`

	// Compress configures the external compressor archives are piped through. It is the
	// config-wide default; a dumptype may replace it wholesale with its own `compress`
	// block (see DumpType.Compress), exactly as encryption overrides.
	Compress CompressConfig `yaml:"compress,omitempty"`

	// Encrypt configures the external encryptor archives are piped through, after
	// compression. It is the config-wide default; a dumptype may replace it wholesale
	// with its own `encrypt` block (see DumpType.Encrypt). Unset = no encryption.
	Encrypt EncryptConfig `yaml:"encrypt,omitempty"`

	// Nice runs orchestrated child processes under `nice -n Nice` for CPU
	// politeness; 0 = no nice.
	Nice int `yaml:"nice,omitempty"`

	// AutoLabel lets a dump label a blank tape automatically instead of requiring
	// an explicit `nb label`. Off by default: explicit labeling is what makes the
	// overwrite guard meaningful. It never clobbers foreign or non-blank media.
	AutoLabel bool `yaml:"auto_label,omitempty"`

	// Parallelism bounds concurrent work within a run.
	Parallelism struct {
		Workers int `yaml:"workers,omitempty"` // concurrent DLE dumps per run (default 1)
	} `yaml:"parallelism,omitempty"`

	// Media is a map of named storage definitions.
	Media map[string]Media `yaml:"media,omitempty"`

	// Archivers is a map of named archiver definitions: a
	// registered archiver type plus its options, referenced by a dumptype. An
	// undeclared name is treated as a bare archiver type with default options, so a
	// zero-config `archiver: gnutar` needs no block here.
	Archivers map[string]Archiver `yaml:"archivers,omitempty"`

	// Sync declares replication rules: each mirrors the landing medium's sealed
	// runs onto a target medium. `nb sync` with no --to runs
	// every rule; `nb sync --to X` is the ad-hoc form and needs no rule.
	Sync []SyncRule `yaml:"sync,omitempty"`

	// Drill configures recovery drills (`nb drill`): the recoverability rehearsal
	// layered on `nb verify`. It mirrors the sync block so a cron line can be
	// `nb dump && nb sync && nb drill --unattended`.
	Drill DrillConfig `yaml:"drill,omitempty"`

	// Notify configures unattended alerting: which channels fire on a run's
	// failure/success, and which receive `nb report --notify` digests. It is what
	// makes a cron-driven backup loud — a failed dump/sync/verify/drill reaches a
	// human. Secrets are referenced by environment variable, never stored here.
	Notify NotifyConfig `yaml:"notify,omitempty"`

	// DumpTypes is a map of named dumptypes: an archiver reference plus per-DLE policy
	// (encryption).
	DumpTypes map[string]DumpType `yaml:"dumptypes,omitempty"`

	// SSH holds the default SSH connection settings applied to every remote host.
	// A per-host `hosts.<name>.ssh` block overrides
	// individual fields; an undeclared remote host uses these as-is.
	SSH SSHConfig `yaml:"ssh,omitempty"`

	// Hosts overrides the SSH defaults for specific source hosts. It is NOT what makes a
	// host remote — any source host that is not `localhost` is remote by default and runs
	// stock tools (tar, and optionally the compressor + gpg) on the client over SSH, with
	// no NBackup software on the client. List a host here only to override its defaults.
	Hosts map[string]HostConfig `yaml:"hosts,omitempty"`

	Sources Sources `yaml:"sources"`
}

// HostConfig overrides per-host settings for one source host. The two kinds are kept
// distinct: connection lives in the `ssh` sub-block; StateDir is host-level and
// archiver-agnostic (where every archiver on this host keeps incremental state); and
// Archivers carries archiver-specific property overrides keyed by archiver type, so a
// per-host gnutar binary path is `archivers.gnutar.tar_path` — the seam that scales to
// a future star/pgsql archiver without growing this struct.
type HostConfig struct {
	SSH       SSHConfig                    `yaml:"ssh,omitempty"`
	StateDir  string                       `yaml:"state_dir,omitempty"` // this host's incremental-state root (overrides Config.StateDir)
	Archivers map[string]map[string]string `yaml:"archivers,omitempty"` // archiver-type → property overrides (e.g. gnutar: {tar_path: …})
}

// RemoteHost returns the effective SSH connection for a host and true when it is remote.
// A DLE is remote by default: anything but `localhost` (or an empty host) is backed up
// over SSH — `hosts:` is needed only to override the defaults for a specific host, not to
// make it remote. The effective config is the top-level `ssh:` defaults with any per-host
// `hosts.<name>.ssh` block field-merged over them.
func (c *Config) RemoteHost(host string) (SSHConfig, bool) {
	if host == "" || host == "localhost" {
		return SSHConfig{}, false // localhost is the only local marker
	}
	eff := c.SSH
	if h, ok := c.Hosts[host]; ok {
		eff = mergeSSH(eff, h.SSH)
	}
	return eff, true
}

// DrillConfig is the `drill:` block: how often each DLE must be drilled, how many to
// drill per run, which copy to read, and how deeply. CLI flags override these.
type DrillConfig struct {
	Window     string `yaml:"window,omitempty"`     // each DLE drilled within this window (default 30d)
	Sample     int    `yaml:"sample,omitempty"`     // DLEs drilled per run (default 1)
	From       string `yaml:"from,omitempty"`       // source medium to drill ("" = the landing medium)
	Tier       string `yaml:"tier,omitempty"`       // checksum | structural | chain | stock (default structural)
	Worm       bool   `yaml:"worm,omitempty"`       // run the WORM/immutability probe
	Unattended bool   `yaml:"unattended,omitempty"` // cron mode: never prompt; skip swap-needing targets
}

// NotifyConfig is the `notify:` block: a set of named backends plus per-outcome
// routing. Failures must be loud, so when on_failure is omitted every configured
// backend fires on failure; success and digest notifications are opt-in. It mirrors
// the declarative shape of the sync/drill blocks.
type NotifyConfig struct {
	OnFailure []string `yaml:"on_failure,omitempty"` // backends to notify on a failed run ("" = all backends)
	OnSuccess []string `yaml:"on_success,omitempty"` // backends to notify on a successful run (default: none)
	Digest    []string `yaml:"digest,omitempty"`     // backends for `nb report --notify` (default: none)

	Backends map[string]NotifyBackend `yaml:"backends,omitempty"`
}

// NotifyBackend is one named notification channel. Connection settings are explicit
// fields (so `KnownFields(true)` rejects a stray key — including a literal
// `password:`/`token:`, which structurally enforces the env-reference rule below).
// Secrets are NEVER stored: they are named environment variables (password_env,
// url_env) resolved at send time, mirroring crypt's orchestrate-don't-hoard stance.
type NotifyBackend struct {
	Type string `yaml:"type,omitempty"` // smtp | sendmail | webhook (a registered notifier name)

	// smtp / sendmail
	Host        string   `yaml:"host,omitempty"`
	Port        int      `yaml:"port,omitempty"`
	From        string   `yaml:"from,omitempty"`
	To          []string `yaml:"to,omitempty"`
	Username    string   `yaml:"username,omitempty"`
	PasswordEnv string   `yaml:"password_env,omitempty"` // env var holding the SMTP password (never the password itself)

	// sendmail
	SendmailPath string `yaml:"sendmail_path,omitempty"` // path to the local sendmail binary (default /usr/sbin/sendmail)

	// webhook
	URL      string            `yaml:"url,omitempty"`      // a non-secret endpoint; prefer url_env for anything secret
	URLEnv   string            `yaml:"url_env,omitempty"`  // env var holding the webhook URL (Slack/Discord/PagerDuty secret)
	Headers  map[string]string `yaml:"headers,omitempty"`  // optional extra HTTP headers
	Template string            `yaml:"template,omitempty"` // optional payload field name for the message (default "text")
}

// SyncRule mirrors one medium's runs onto another. Selection bounds keep an
// expensive target (tape, object store) to a recent window; an unbounded rule
// replicates everything. The source defaults to the landing medium (the same
// medium `nb copy` streams from) but may be any other medium via `from`.
type SyncRule struct {
	To   string `yaml:"to,omitempty"`   // target medium name (required; must differ from the source)
	From string `yaml:"from,omitempty"` // source medium name ("" = the landing medium)
	Last int    `yaml:"last,omitempty"` // copy only the N most recent runs (0 = all)
}

// DefaultDrillWindow is the assumed drill coverage window when `drill.window` is
// unset: every DLE should be drilled at least this often.
const DefaultDrillWindow = 30 * 24 * time.Hour

// DefaultDrillSample is the number of DLEs drilled per run when `drill.sample` is
// unset — a small, risk-biased rotation so routine drills stay cheap.
const DefaultDrillSample = 1

// DefaultDrillTier is the drill tier assumed when `drill.tier` is unset — the
// no-write structural tier fit for routine drills.
const DefaultDrillTier = "structural"

// DrillWindow returns the drill coverage window (default DefaultDrillWindow).
// Validate (via validateDrill) already parsed and accepted any non-empty
// Drill.Window, so the parse here cannot fail; a non-positive window is not
// rejected up-front, though, so it still falls back to the default.
func (c *Config) DrillWindow() time.Duration {
	if c.Drill.Window == "" {
		return DefaultDrillWindow
	}
	d, _ := sizeutil.ParseDuration(c.Drill.Window)
	if d > 0 {
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

// DrillTierName returns the configured drill tier token, defaulting to
// DefaultDrillTier (the no-write tier fit for routine drills).
func (c *Config) DrillTierName() string {
	if c.Drill.Tier != "" {
		return c.Drill.Tier
	}
	return DefaultDrillTier
}

// DLEs returns the configured backup sources.
func (c *Config) DLEs() []DLE { return c.Sources }

// landingDefined reports an error if any `landing` entry names a medium that is
// not defined. This holds even when the media map is empty: a config that sets
// `landing:` but omits its `media:` block is a misconfig, not a cue to silently
// synthesize a default medium (which would discard the requested landing and write
// to ./nbackup-catalog). An empty landing is not an error here — LandingNames resolves
// it via the sole-medium fallback. (Validate runs the fuller validateLandingList,
// which repeats this check with holding/duplicate rules; this one guards the
// runtime resolve of a config that skipped Validate.)
func (c *Config) landingDefined() error {
	for _, name := range c.Landing {
		if _, ok := c.Media[name]; !ok {
			return fmt.Errorf("landing %q is not a defined medium", name)
		}
	}
	return nil
}

// LandingNames resolves the landing route: the configured `landing` list, or the
// sole medium when none is set and exactly one is defined. The first name is the
// primary (see MediumList).
func (c *Config) LandingNames() ([]string, error) {
	if len(c.Landing) > 0 {
		if err := c.landingDefined(); err != nil {
			return nil, err
		}
		return c.Landing, nil
	}
	if len(c.Media) == 1 {
		for name := range c.Media {
			return []string{name}, nil
		}
	}
	return nil, fmt.Errorf("no landing medium selected (set `landing:` to a media name, or a list of them)")
}

// LandingName resolves the primary landing medium — the single "the landing" every
// per-medium consumer (accounting, read preference, sync's default source) uses.
func (c *Config) LandingName() (string, error) {
	names, err := c.LandingNames()
	if err != nil {
		return "", err
	}
	return names[0], nil
}

// LandingsFor resolves the media a DLE's archives land on: its dumptype's `landing`
// override when set, else the config-wide LandingNames. A dumptype override routes
// different sources to different media (e.g. bulk media to cheap cloud, databases to
// fast disk) within one run; a multi-name route writes the archive to every listed
// medium. The override is validated against `media` at load
// (validateDumpTypeLandings), so a run-time resolve trusts it.
func (c *Config) LandingsFor(d DLE) ([]string, error) {
	if l := c.ResolveDumpType(d.DumpTypeName()).Landing; len(l) > 0 {
		return l, nil
	}
	return c.LandingNames()
}

// LandingFor resolves a DLE's primary landing — LandingsFor's first entry, for
// consumers that need the one accounted medium.
func (c *Config) LandingFor(d DLE) (string, error) {
	names, err := c.LandingsFor(d)
	if err != nil {
		return "", err
	}
	return names[0], nil
}

// HoldingMedia returns the names of every medium marked `holding: true`, sorted — the fast
// scratch buffers dumps flow through on the way to the landing(s). Empty when no medium is a
// holding disk (the normal direct-to-landing run). Dumpers spread their writes across these; the
// drains copy each staged archive to every landing on its route. The order is deterministic so a
// run's disk allocation and Flush's drain order are reproducible.
func (c *Config) HoldingMedia() []string {
	var names []string
	for name, m := range c.Media {
		if m.Holding {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ResolveDumpType returns the named dumptype (the zero value for an unknown name;
// its empty Archiver resolves to DefaultArchiver via ResolveArchiver).
func (c *Config) ResolveDumpType(name string) DumpType {
	return c.DumpTypes[name]
}

// DefaultArchiver is the archiver assumed when a dumptype names none. It is both the
// default archiver name and (for an undeclared name) its type.
const DefaultArchiver = "gnutar"

// ResolveArchiver returns the archiver definition for a name: the declared definition
// (its Type defaulting to the name itself, so a bare `archivers: {gnutar: {}}`
// works), or — for an undeclared name — a bare definition of that type with no
// options, so a zero-config `archiver: gnutar` needs no block. An empty name
// resolves to DefaultArchiver.
func (c *Config) ResolveArchiver(name string) Archiver {
	if name == "" {
		name = DefaultArchiver
	}
	if d, ok := c.Archivers[name]; ok {
		if d.Type == "" {
			d.Type = name
		}
		return d
	}
	return Archiver{Type: name}
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

// CompressionFor returns the compression settings for a dumptype: its own `compress`
// block if it sets one, otherwise the config-wide default. The override replaces the
// default wholesale — fields are not merged — exactly like EncryptionFor.
func (c *Config) CompressionFor(dtName string) CompressConfig {
	if dt, ok := c.DumpTypes[dtName]; ok && dt.Compress != nil {
		return *dt.Compress
	}
	return c.Compress
}

// CompressScheme returns the config-wide default compression scheme, defaulting to
// zstd. Per-dumptype dumps resolve their scheme via CompressionFor; this is the
// baseline the server-side decode/check uses.
func (c *Config) CompressScheme() string {
	return c.Compress.SchemeName()
}

// Workers returns the number of concurrent DLE dumps per run (default 1).
func (c *Config) Workers() int {
	if c.Parallelism.Workers > 1 {
		return c.Parallelism.Workers
	}
	return 1
}

// DefaultCycle is the dump cycle assumed when `cycle` is unset.
const DefaultCycle = 7 * 24 * time.Hour

// DefaultAtomSize is the atomic shape's default atom size (compressed bytes per sealed
// part). 10 GiB matches the cloud media's default slice part_size, so an encrypted
// archive lands as about as many objects as an unencrypted one does today.
const DefaultAtomSize = 10 << 30

// AtomSizeBytes resolves the atom size for one dumptype: its own part_size, else the
// top-level part_size, else DefaultAtomSize. Consulted only for atomic-shape pipelines
// ("an atomic archive brings its own part size"); Validate already parsed and rejected
// bad values, so the parses here cannot fail.
func (c *Config) AtomSizeBytes(dumptype string) int64 {
	if dt, ok := c.DumpTypes[dumptype]; ok && dt.PartSize != "" {
		if n, _ := sizeutil.ParseBytes(dt.PartSize); n > 0 {
			return n
		}
	}
	if c.PartSize != "" {
		if n, _ := sizeutil.ParseBytes(c.PartSize); n > 0 {
			return n
		}
	}
	return DefaultAtomSize
}

// DefaultFrameSize is the framed shape's default decode-restart interval (raw bytes).
// 256 MiB keeps the ratio cost negligible (+0.03% measured at far smaller frames) while
// a single-file ranged restore fetches at most a frame's worth of extra bytes.
const DefaultFrameSize = 256 << 20

// FrameSizeBytes returns the framed shape's decode-restart interval (default
// DefaultFrameSize). Validate already parsed and rejected a bad or non-positive
// frame_size, so the parse here cannot fail.
func (c *Config) FrameSizeBytes() int64 {
	if c.FrameSize == "" {
		return DefaultFrameSize
	}
	n, _ := sizeutil.ParseBytes(c.FrameSize)
	if n > 0 {
		return n
	}
	return DefaultFrameSize
}

// CycleDuration returns the dump cycle as a duration (default DefaultCycle).
// Validate already parsed any non-empty Cycle and rejected a non-positive one,
// so the parse here cannot fail and the result is always positive.
func (c *Config) CycleDuration() time.Duration {
	if c.Cycle == "" {
		return DefaultCycle
	}
	d, _ := sizeutil.ParseDuration(c.Cycle)
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

// DefaultBumpPercent is the level-bump savings threshold assumed when
// `bump_percent` is unset: an incremental climbs to the next level only when that
// saves at least this percent of the full-dump size. Five percent is a deliberately
// real saving — so level 1 stays the common case
// and deeper levels are earned, not reached automatically.
const DefaultBumpPercent = 5.0

// BumpPercent returns the level-bump savings threshold in percent (default
// DefaultBumpPercent). Zero or negative falls back to the default.
func (c *Config) BumpPercent() float64 {
	if c.BumpPct <= 0 {
		return DefaultBumpPercent
	}
	return c.BumpPct
}

// MinAgeFor returns a medium's effective retention floor: its configured
// minimum_age, or the dump cycle when unset. Defaulting to one cycle keeps the
// "yesterday must not overwrite last month" safety without a knob — a run stays
// retainable for at least the window in which it is still a recovery base.
func (c *Config) MinAgeFor(m Media) time.Duration {
	// An explicit minimum_age is honored as given — including 0, which means "no age
	// floor / capacity-only retention" (only the live recovery chain still protects).
	// An omitted key defaults to one cycle, keeping "yesterday must not overwrite last
	// month" safe without a knob. Validate already rejected a negative value.
	if m.MinimumAge != "" {
		age, _ := m.MinAge()
		return age
	}
	return c.CycleDuration()
}

// DefaultWorkdir is where the catalog cache lives when `workdir` is unset. It is
// deliberately independent of any storage medium: the catalog is a cache over the
// whole pool, not a thing owned by one medium.
const DefaultWorkdir = "nbackup-catalog"

// WorkdirPath returns the catalog's own operational-state directory (the run
// cache), independent of any storage medium. It defaults to DefaultWorkdir when
// `workdir` is unset.
func (c *Config) WorkdirPath() string {
	if c.Workdir != "" {
		return c.Workdir
	}
	return DefaultWorkdir
}

// DefaultStateDir is the host's incremental-state library root when neither a per-host
// `hosts.<h>.state_dir` nor the top-level `state_dir` is set. It is deliberately a
// dedicated location *beside* the catalog workdir, not beneath it: the workdir is a
// disposable cache (rebuildable from the media) while the state library is the one piece
// of precious, non-rebuildable state, so nesting it under the wipe-and-rebuild workdir
// would invite its loss. It is relative, so it resolves under the server's cwd for a
// local host and under the backup user's home on a client.
const DefaultStateDir = "nbackup-state"

// StatePath returns the fleet-wide default incremental-state root, defaulting to
// DefaultStateDir when `state_dir` is unset. It is the host-level fallback used by
// StateDirFor when a host sets no override.
func (c *Config) StatePath() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return DefaultStateDir
}

// DefaultSecretsDir is the pool-side credential root when `secrets_dir` is unset. Like
// DefaultStateDir it is a dedicated location BESIDE the catalog workdir, not beneath it: a
// login token (the gdrive OAuth token) is precious, non-rebuildable state, so nesting it
// under the wipe-and-rebuild workdir would invite its loss. Relative, so it resolves under
// the server's cwd — the same base the workdir defaults to.
const DefaultSecretsDir = "nbackup-secrets"

// SecretsPath returns the pool-side credential root, defaulting to DefaultSecretsDir when
// `secrets_dir` is unset. A medium's login bootstrap writes under it and the medium reads
// its credential back from it (see the gdrive medium).
func (c *Config) SecretsPath() string {
	if c.SecretsDir != "" {
		return c.SecretsDir
	}
	return DefaultSecretsDir
}

// StateDirFor returns the incremental-state root for a host: the host's own
// `hosts.<host>.state_dir` when set, else the fleet-wide StatePath. The path is a
// location on the host where the archiver runs (the server for a local host, the client
// for a remote one); a relative default resolves there, so the server's path never leaks
// onto a client.
func (c *Config) StateDirFor(host string) string {
	if h, ok := c.Hosts[host]; ok && h.StateDir != "" {
		return h.StateDir
	}
	return c.StatePath()
}

// ArchiverOverrides returns the per-host property overrides for an archiver type on a
// host (e.g. gnutar's tar_path on a client whose binary lives off the default PATH), or
// nil when the host sets none. They merge over the archiver definition's options.
func (c *Config) ArchiverOverrides(host, archiverType string) map[string]string {
	if h, ok := c.Hosts[host]; ok {
		return h.Archivers[archiverType]
	}
	return nil
}
