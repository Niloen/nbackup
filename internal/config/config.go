// Package config loads and validates NBackup configuration files. It also
// defines the configured domain entities — DLEs (backup sources), named media
// definitions, named archiver definitions, and dumptypes
// (an archiver reference plus per-DLE policy).
package config

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"gopkg.in/yaml.v3"
)

// DefaultDumpType is used by DLEs that do not name one.
const DefaultDumpType = "default"

// DefaultCompress is the compression scheme assumed when compress.scheme is unset.
const DefaultCompress = "zstd"

// DefaultArchiver is the archiver assumed when a dumptype names none. It is both the
// default archiver name and (for an undeclared name) its type.
const DefaultArchiver = "gnutar"

// DefaultWorkdir is where the catalog cache lives when `workdir` is unset. It is
// deliberately independent of any storage medium: the catalog is a cache over the
// whole pool, not a thing owned by one medium.
const DefaultWorkdir = "nbackup-catalog"

// DefaultStateDir is the host's incremental-state library root when neither a per-host
// `hosts.<h>.state_dir` nor the top-level `state_dir` is set. It is deliberately a
// dedicated location *beside* the catalog workdir, not beneath it: the workdir is a
// disposable cache (rebuildable from the media) while the state library is the one piece
// of precious, non-rebuildable state, so nesting it under the wipe-and-rebuild workdir
// would invite its loss. It is relative, so it resolves under the server's cwd for a
// local host and under the backup user's home on a client.
const DefaultStateDir = "nbackup-state"

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

	// Landing names the media definition where runs are created.
	Landing string `yaml:"landing,omitempty"`

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

// SSHConfig is the SSH connection to a client. NBackup stores no secret: the key comes
// from the operator's ssh config/agent (IdentityFile is a path, not a key), exactly as
// cloud and gpg credentials are handled. It carries connection settings only — the
// client-side tar path and the .snar library root are host/archiver concerns, set via
// HostConfig.Archivers and HostConfig.StateDir respectively.
type SSHConfig struct {
	User         string   `yaml:"user,omitempty"`
	Port         string   `yaml:"port,omitempty"`
	IdentityFile string   `yaml:"identity_file,omitempty"`
	Options      []string `yaml:"options,omitempty"` // extra raw ssh options, e.g. ["-o","StrictHostKeyChecking=accept-new"]
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

// mergeSSH overlays over onto base: each set field in over wins, unset fields inherit
// base. So a per-host block can override just the user while inheriting the global
// identity_file/options.
func mergeSSH(base, over SSHConfig) SSHConfig {
	if over.User != "" {
		base.User = over.User
	}
	if over.Port != "" {
		base.Port = over.Port
	}
	if over.IdentityFile != "" {
		base.IdentityFile = over.IdentityFile
	}
	if over.Options != nil {
		base.Options = over.Options
	}
	return base
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

// validDrillTiers is the accepted set for the drill tier token (kept here so config
// validation needs no dependency on package drill, which depends on no config).
var validDrillTiers = map[string]bool{"": true, "checksum": true, "structural": true, "chain": true, "stock": true}

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

// validNotifyTypes is the accepted set for a backend's type (kept here so config
// validation needs no dependency on package notify, which depends on config).
var validNotifyTypes = map[string]bool{"smtp": true, "sendmail": true, "webhook": true}

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
// dumptype, not the entry.
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

// MarshalYAML emits the grouped dumptype -> {host: [paths]} form UnmarshalYAML
// reads, so a programmatically built config (`nb init`) round-trips through the
// real loader instead of serializing the internal flat DLE list.
func (s Sources) MarshalYAML() (any, error) {
	grouped := map[string]map[string][]string{}
	for _, d := range s {
		dt := d.DumpTypeName()
		if grouped[dt] == nil {
			grouped[dt] = map[string][]string{}
		}
		grouped[dt][d.Host] = append(grouped[dt][d.Host], d.Path)
	}
	return grouped, nil
}

// Media is one named storage definition: a type, capacity/retention policy for
// this medium, and type-specific connection parameters (e.g. disk has
// "path", s3 has "bucket"). Capacity and retention are per-medium because each
// store has its own space and reuse cadence.
type Media struct {
	Type       string `yaml:"type,omitempty"`
	Capacity   string `yaml:"capacity,omitempty"`    // space NBackup may use here, e.g. "20TB" ("" = unbounded)
	MinimumAge string `yaml:"minimum_age,omitempty"` // retention floor before a run may be retired here (default: one cycle)
	// Holding marks this medium as a holding disk: a fast scratch buffer the dump flows
	// through on the way to the landing. Dumps land here in parallel, then drains copy
	// each committed archive to the landing and reclaim it — so the landing's drive
	// runs at disk speed and a small disk feeds a much larger landing. Must be a disk/cloud
	// medium (per-archive reclaim, and the only sink safe for concurrent dumpers), never the
	// landing itself. `capacity` bounds the in-flight back-pressure.
	Holding    bool   `yaml:"holding,omitempty"`
	Appendable *bool  `yaml:"appendable,omitempty"` // pack many runs per volume (default) vs one run per volume
	Throughput string `yaml:"throughput,omitempty"` // bandwidth cap to/from this medium, e.g. "50MB/s" ("" = uncapped); network politeness, the read/write peer of nice
	// Writers caps how many archives may be written to this medium at once — one lever for
	// the medium's write concurrency, counted the same whether the write is a dumper's
	// direct dump, a drain copying a staged archive off the holding disk, or (for a holding
	// medium) a dumper staging onto it. 0 (unset) means the medium's natural width: a serial
	// library's drive count, else the run's worker count. A serial library never exceeds its
	// drives regardless (two archives cannot interleave on one rolling volume). Set 1 on a
	// spinning disk to keep its writes sequential (Amanda's taper-parallel-write).
	Writers int               `yaml:"writers,omitempty"`
	Cost    *CostConfig       `yaml:"cost,omitempty"` // optional pricing overrides; absent = inferred from type/url
	Params  map[string]string `yaml:",inline"`        // type-specific connection params (path, bucket, tapes, ...)
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
	opts := m.paramsCopy()
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

// paramsCopy returns a fresh, always-non-nil copy of the medium's inline
// connection params, the base map the ProfileOptions/CostOptions factories
// flatten further fields onto.
func (m Media) paramsCopy() map[string]string {
	opts := maps.Clone(m.Params)
	if opts == nil {
		opts = map[string]string{}
	}
	return opts
}

func putRate(opts map[string]string, key string, v *float64) {
	if v != nil {
		opts[key] = strconv.FormatFloat(*v, 'f', -1, 64)
	}
}

// IsAppendable reports whether a volume may accumulate many runs until full
// (Bacula-style, the default). When false, a volume holds a single run before it
// must be changed. Address-identified media ignore it.
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
// backup. Concurrent workers to one medium share the single budget,
// since a run writes a single landing medium.
func (m Media) ThroughputBytes() (int64, error) {
	if m.Throughput == "" {
		return 0, nil
	}
	return sizeutil.ParseRate(m.Throughput)
}

// ProfileOptions flattens the medium's capacity field and connection params into
// the generic option map a media.Profile factory consumes.
func (m Media) ProfileOptions() map[string]string {
	opts := m.paramsCopy()
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

// SyncRule mirrors one medium's runs onto another. Selection bounds keep an
// expensive target (tape, object store) to a recent window; an unbounded rule
// replicates everything. The source defaults to the landing medium (the same
// medium `nb copy` streams from) but may be any other medium via `from`.
type SyncRule struct {
	To   string `yaml:"to,omitempty"`   // target medium name (required; must differ from the source)
	From string `yaml:"from,omitempty"` // source medium name ("" = the landing medium)
	Last int    `yaml:"last,omitempty"` // copy only the N most recent runs (0 = all)
}

// DumpType names an archiver and carries per-DLE policy, referenced by DLEs. The
// archiver (how the stream is produced — program + content-independent options) and
// the policy here (what to skip and what to do with the stream) are deliberately
// split. Excludes live here, not on the archiver: skipping `*.log` is
// a content decision about the source, not a property of how tar runs.
type DumpType struct {
	Archiver string          `yaml:"archiver"`           // named archiver definition ("" = DefaultArchiver)
	Exclude  []string        `yaml:"exclude,omitempty"`  // patterns to skip (passed to the archiver per dump)
	Encrypt  *EncryptConfig  `yaml:"encrypt,omitempty"`  // nil = inherit the config-wide default; set = replace it wholesale (no field merge)
	Compress *CompressConfig `yaml:"compress,omitempty"` // nil = inherit the config-wide default; set = replace it wholesale (no field merge) — the peer of Encrypt
	Landing  string          `yaml:"landing,omitempty"`  // medium this dumptype's DLEs land on; "" = the config-wide `landing`. Routes different sources to different media (cheap cloud vs fast disk vs tape) within one run.
}

// Archiver is a named dump-program definition: a
// registered archiver type plus its content-independent options, referenced by a
// dumptype. Options are archiver-specific (gnutar's tar_path, one-file-system, …) and
// flow through the inline map, so KnownFields does not reject them. A per-host override
// of any option lives in `hosts.<h>.archivers.<type>`. (Excludes are a dumptype concern,
// and the incremental-state root is the host's state_dir — neither is an archiver
// option.)
type Archiver struct {
	Type    string            `yaml:"type,omitempty"` // registered archiver type ("" = the definition's name)
	Options map[string]string `yaml:",inline"`        // archiver-specific options
}

// CompressConfig selects a compression scheme, its tuning, and where it runs. It is
// the write-side peer of EncryptConfig: the top-level `compress:` block is the
// config-wide default, and a dumptype may replace it wholesale with its own block
// (no field merge), so a dumptype can pick a different algorithm/level as well as a
// different location. The scheme is a compiled name (zstd|gzip|none), recorded
// per-archive so restore reverses it from the artifact alone.
type CompressConfig struct {
	Scheme  string `yaml:"scheme,omitempty"`  // zstd | gzip | none (default zstd)
	Level   int    `yaml:"level,omitempty"`   // compression level; 0 = scheme default
	Threads int    `yaml:"threads,omitempty"` // worker threads where supported; 0 = scheme default
	Program string `yaml:"program,omitempty"` // optional binary override (name or path)

	// At selects where compression runs, for a remote DLE: "server" (default — on the
	// NBackup host) or "client" (on the source client, so only compressed bytes cross
	// the wire). Encryption is downstream of compression, so an encrypt.at: client
	// requires this to be "client" too (validated at load). Local DLEs ignore it.
	At string `yaml:"at,omitempty"`
}

// SchemeName returns the configured scheme, defaulting to DefaultCompress (zstd).
func (cc CompressConfig) SchemeName() string {
	if cc.Scheme != "" {
		return cc.Scheme
	}
	return DefaultCompress
}

// EncryptConfig selects an encryption scheme and its key reference. The scheme is
// a compiled name (gpg|none); the key reference (recipient or passphrase file) is
// passed to the encryptor but never recorded — gpg owns the key material.
type EncryptConfig struct {
	Scheme         string `yaml:"scheme,omitempty"`          // gpg | none (default none)
	Recipient      string `yaml:"recipient,omitempty"`       // gpg public-key recipient (asymmetric)
	PassphraseFile string `yaml:"passphrase_file,omitempty"` // gpg symmetric passphrase file
	Program        string `yaml:"program,omitempty"`         // optional binary override (name or path)

	// At selects where encryption runs, for a remote
	// DLE: "server" (default — on the NBackup host) or "client" (on the source client,
	// so only ciphertext crosses the wire and plaintext never leaves the client). Since
	// encryption is downstream of compression, At=="client" requires the dumptype's
	// Compress=="client" (validated at load). With a public-key recipient only the
	// public key need be on the client; the private key resolves the ciphertext wherever
	// it lives (the asymmetric/untrusted-server postures).
	At string `yaml:"at,omitempty"`
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
	Host     string `yaml:"host,omitempty"`
	Path     string `yaml:"path"`
	DumpType string `yaml:"dumptype"`
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// yamlUnknownField rewrites go-yaml's "field X not found in type <T>" — which leaks
// an internal Go type name (a named `pkg.Type`, or a whole anonymous `struct { … }`
// literal for an inline block) — into a user-facing "unknown key X". The type part
// is matched to end-of-line so an anonymous struct's body (it contains spaces) is
// dropped wholesale rather than leaving its field dump behind.
var yamlUnknownField = regexp.MustCompile(`field (\S+) not found in type .*`)

// secretKeys are config keys a user might reach for to store a secret inline; the
// config never holds secrets, so the error points them at the env-var indirection.
var secretKeys = map[string]string{
	"password": "password_env", "passwd": "password_env",
	"token": "url_env", "secret": "url_env", "url": "url_env",
}

// cleanYAMLError turns go-yaml's decode error into a config-author-facing message:
// it drops the "yaml: unmarshal errors:" banner and the internal Go type name,
// leaving the line number and the offending key (e.g. `line 1: unknown key "cyle"`).
// A rejected secret-looking key gets an extra hint toward the env-var reference.
func cleanYAMLError(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "yaml: unmarshal errors:\n", "")
	s = yamlUnknownField.ReplaceAllString(s, `unknown key "$1"`)
	s = strings.TrimSpace(s)
	for key, envKey := range secretKeys {
		if strings.Contains(s, `unknown key "`+key+`"`) {
			s += fmt.Sprintf("\n(secrets are never stored in the config — reference the environment-variable name instead, e.g. `%s`)", envKey)
			break
		}
	}
	return s
}

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

// ID returns the host:path identity of a DLE, e.g. "app01:/home".
// This is what users see in reports and type for `--dle`/`setdisk`; the slug from
// Name() stays internal (filenames, snapshot state, catalog keys).
func (d DLE) ID() string {
	return d.Host + ":" + d.Path
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
	// archiver options still flow through their inline maps, so connection keys
	// (path, url, bays, tar_path, …) are unaffected.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %s", path, cleanYAMLError(err))
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
				known := []string{DefaultDumpType}
				for name := range c.DumpTypes {
					if name != DefaultDumpType {
						known = append(known, name)
					}
				}
				sort.Strings(known)
				return fmt.Errorf("source %s: unknown dumptype %q (known: %s)", s.ID(), dt, strings.Join(known, ", "))
			}
		}
		if err := c.validateTransformPlacement(s); err != nil {
			return err
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
		if age, err := m.MinAge(); err != nil {
			return fmt.Errorf("media %s: minimum_age: %w", name, err)
		} else if m.MinimumAge != "" && age <= 0 {
			// A non-positive floor would fall through to the one-cycle default in
			// MinAgeFor, silently ignoring the user's explicit value. Reject it so the
			// mistake surfaces; omitting the key is the way to ask for the default.
			return fmt.Errorf("medium %q: minimum_age must be positive (omit it to default to one cycle)", name)
		}
		if _, err := m.ThroughputBytes(); err != nil {
			return fmt.Errorf("media %s: throughput: %w", name, err)
		}
		if m.Writers < 0 {
			return fmt.Errorf("media %s: writers must be positive (omit it to default to the medium's natural width)", name)
		}
	}
	if err := c.validateArchivers(); err != nil {
		return err
	}
	if err := c.validateDumpTypeArchivers(); err != nil {
		return err
	}
	if err := c.validateDumpTypeLandings(); err != nil {
		return err
	}
	if err := c.validateHolding(); err != nil {
		return err
	}
	if err := c.validateSync(); err != nil {
		return err
	}
	if err := c.validateDrill(); err != nil {
		return err
	}
	if err := c.validateNotify(); err != nil {
		return err
	}
	return nil
}

// validateDumpTypeLandings rejects a dumptype whose `landing` override names a medium
// that is not defined — the per-dumptype peer of landingDefined, so a routing typo is
// caught at load rather than mid-run. A holding medium is not a valid landing (it is a
// write-path buffer, not an authoritative destination).
func (c *Config) validateDumpTypeLandings() error {
	for name, dt := range c.DumpTypes {
		if dt.Landing == "" {
			continue
		}
		m, ok := c.Media[dt.Landing]
		if !ok {
			return fmt.Errorf("dumptype %q: landing %q is not a defined medium", name, dt.Landing)
		}
		if m.Holding {
			return fmt.Errorf("dumptype %q: landing %q is a holding medium, not a landing", name, dt.Landing)
		}
	}
	return nil
}

// validateDumpTypeArchivers rejects a dumptype that names an archiver which is neither a
// defined `archivers:` entry nor a registered bare type. Without this the bad reference falls
// through to Open, whose "unknown archiver" lists the registered TYPE (gnutar) — useless when
// a reference must name a *defined* archiver. The hint lists the names actually defined.
func (c *Config) validateDumpTypeArchivers() error {
	for name, dt := range c.DumpTypes {
		ref := dt.Archiver
		if ref == "" {
			continue // empty resolves to DefaultArchiver, always registered
		}
		if _, defined := c.Archivers[ref]; defined {
			continue
		}
		if _, isType := archiver.KnownOptions(ref); isType {
			continue // an undeclared name that is itself a registered bare type is allowed
		}
		known := make([]string, 0, len(c.Archivers))
		for n := range c.Archivers {
			known = append(known, n)
		}
		sort.Strings(known)
		hint := "none defined — add an archivers: entry, or use a registered type like " + DefaultArchiver
		if len(known) > 0 {
			hint = "defined: " + strings.Join(known, ", ")
		}
		return fmt.Errorf("dumptype %q: unknown archiver %q (%s)", name, ref, hint)
	}
	return nil
}

// validateArchivers checks every archiver's inline options against the option keys its
// type accepts (declared in the archiver registry) — both the named definitions and any
// per-host override of them (`hosts.<h>.archivers.<type>`). Options ride an inline map, so
// YAML's KnownFields check can't reach them; without this a typo'd option (e.g.
// `one-file-sytem`) would be silently dropped, quietly disabling a safety-relevant flag. An
// unregistered type is left to fail at Open with its own "unknown archiver".
func (c *Config) validateArchivers() error {
	for name, def := range c.Archivers {
		typeName := def.Type
		if typeName == "" {
			typeName = name
		}
		if err := validateArchiverOptions(fmt.Sprintf("archivers.%s", name), typeName, def.Options); err != nil {
			return err
		}
	}
	// Per-host overrides are keyed by archiver type, so the key is the type directly.
	for host, h := range c.Hosts {
		for typeName, overrides := range h.Archivers {
			if err := validateArchiverOptions(fmt.Sprintf("hosts.%s.archivers.%s", host, typeName), typeName, overrides); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateArchiverOptions rejects any option key the archiver type does not accept,
// naming the offending key and the accepted set. An unregistered type is skipped
// (left to fail at Open). label is the config location for the error message.
func validateArchiverOptions(label, typeName string, options map[string]string) error {
	known, ok := archiver.KnownOptions(typeName)
	if !ok {
		return nil
	}
	accepted := make(map[string]bool, len(known))
	for _, k := range known {
		accepted[k] = true
	}
	for key := range options {
		if !accepted[key] {
			sorted := append([]string(nil), known...)
			sort.Strings(sorted)
			return fmt.Errorf("%s: unknown option %q (accepted: %s)", label, key, strings.Join(sorted, ", "))
		}
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
			return fmt.Errorf("notify: backend %q: unknown type %q (known: smtp, sendmail, webhook)", name, b.Type)
		}
		switch b.Type {
		case "smtp":
			if b.Host == "" || b.From == "" || len(b.To) == 0 {
				return fmt.Errorf("notify: smtp backend %q requires host, from, and at least one recipient (to)", name)
			}
		case "sendmail":
			if b.From == "" || len(b.To) == 0 {
				return fmt.Errorf("notify: sendmail backend %q requires from and at least one recipient (to)", name)
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

// validateSync checks the optional `sync:` rules: each names a defined target
// medium (and source, when given), a source distinct from the target (the
// source defaulting to the landing medium), and a non-negative `last` window.
func (c *Config) validateSync() error {
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
// "structural" (the no-write tier fit for routine drills).
func (c *Config) DrillTierName() string {
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

// LandingFor resolves the medium a DLE's archives land on: its dumptype's `landing`
// override when set, else the config-wide LandingName. A dumptype override routes
// different sources to different media (e.g. bulk media to cheap cloud, databases to
// fast disk) within one run. The override is validated against `media` at load
// (validateDumpTypeLandings), so a run-time resolve trusts it.
func (c *Config) LandingFor(d DLE) (string, error) {
	if l := c.ResolveDumpType(d.DumpTypeName()).Landing; l != "" {
		return l, nil
	}
	return c.LandingName()
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

// HoldingMedia returns the names of every medium marked `holding: true`, sorted — the fast
// scratch buffers dumps flow through on the way to the landing. Empty when no medium is a holding
// disk (the normal direct-to-landing run). Dumpers spread their writes across these; the drain
// copies them all to the one landing. The order is deterministic so a run's disk allocation and
// Flush's drain order are reproducible.
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

// validateHolding checks the structural rule of the holding-disk marker: a holding medium must
// not be the landing (the holding disk buffers a different landing). Several media may be holding
// disks. Whether a medium's type actually supports a holding disk (concurrent writes + per-archive
// reclaim) is a media-layer capability the engine checks where the media registry is wired —
// config stays free of medium-type knowledge.
func (c *Config) validateHolding() error {
	landing, landErr := c.LandingName()
	for name, m := range c.Media {
		if !m.Holding {
			continue
		}
		if landErr == nil && name == landing {
			return fmt.Errorf("media %s is both the landing and a holding disk — the holding disk buffers a different landing", name)
		}
	}
	return nil
}

// ResolveDumpType returns the named dumptype (the zero value for an unknown name;
// its empty Archiver resolves to DefaultArchiver via ResolveArchiver).
func (c *Config) ResolveDumpType(name string) DumpType {
	return c.DumpTypes[name]
}

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

// validateTransformPlacement checks a source's compress/encrypt location settings: the
// values are server|client, encrypt.at: client requires compress.at: client (encryption is
// downstream of compression — otherwise plaintext would cross the wire), and either
// "client" requires the host to be configured under hosts: (a local DLE has nowhere else
// to run the transform).
func (c *Config) validateTransformPlacement(s DLE) error {
	dt := s.DumpTypeName()
	compressAt := c.CompressionFor(dt).At
	encAt := c.EncryptionFor(dt).At
	for what, v := range map[string]string{"compress.at": compressAt, "encrypt.at": encAt} {
		if v != "" && v != "server" && v != "client" {
			return fmt.Errorf("source %s: %s must be \"server\" or \"client\", got %q", s.Name(), what, v)
		}
	}
	if encAt == "client" && compressAt != "client" {
		return fmt.Errorf("source %s: encrypt.at: client requires compress.at: client (encryption is downstream of compression)", s.Name())
	}
	if _, remote := c.RemoteHost(s.Host); !remote && (compressAt == "client" || encAt == "client") {
		return fmt.Errorf("source %s: compress/encrypt \"client\" requires a remote host, but %q is local", s.Name(), s.Host)
	}
	return nil
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
	// Validate already parsed m.MinimumAge and rejected a non-positive explicit
	// value, so MinAge here is either a positive floor or 0 (the key was omitted);
	// 0 falls through to the one-cycle default.
	age, _ := m.MinAge()
	if age > 0 {
		return age
	}
	return c.CycleDuration()
}

// WorkdirPath returns the catalog's own operational-state directory (the run
// cache), independent of any storage medium. It defaults to DefaultWorkdir when
// `workdir` is unset.
func (c *Config) WorkdirPath() string {
	if c.Workdir != "" {
		return c.Workdir
	}
	return DefaultWorkdir
}

// StatePath returns the fleet-wide default incremental-state root, defaulting to
// DefaultStateDir when `state_dir` is unset. It is the host-level fallback used by
// StateDirFor when a host sets no override.
func (c *Config) StatePath() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return DefaultStateDir
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
