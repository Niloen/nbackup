// This file holds the configured domain entities Config references — DLEs and
// their grouped Sources form, named media definitions, dumptypes, archiver
// definitions, SSH connection settings, and the compress/encrypt policy blocks.

package config

import (
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/sizeutil"
	"gopkg.in/yaml.v3"
)

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

// DefaultCompress is the compression scheme assumed when compress.scheme is unset.
const DefaultCompress = "zstd"

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

// DefaultDumpType is used by DLEs that do not name one.
const DefaultDumpType = "default"

// DumpTypeName returns the DLE's dumptype, defaulting to "default".
func (d DLE) DumpTypeName() string {
	if d.DumpType != "" {
		return d.DumpType
	}
	return DefaultDumpType
}
