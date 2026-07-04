package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

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
		} else if m.MinimumAge != "" && age < 0 {
			// A negative floor is nonsense; 0 is allowed and means "no age floor —
			// capacity-only retention" (only the live recovery chain still protects).
			// Omitting the key is the way to ask for the one-cycle default.
			return fmt.Errorf("medium %q: minimum_age must not be negative (use 0 for no age floor / capacity-only retention, or omit it to default to one cycle)", name)
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

// validNotifyTypes is the accepted set for a backend's type (kept here so config
// validation needs no dependency on package notify, which depends on config).
var validNotifyTypes = map[string]bool{"smtp": true, "sendmail": true, "webhook": true}

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
		// Rules are numbered from 1 in messages — operators count list entries from one.
		n := i + 1
		if r.To == "" {
			return fmt.Errorf("sync rule %d: `to` is required", n)
		}
		if len(c.Media) > 0 {
			if _, ok := c.Media[r.To]; !ok {
				return fmt.Errorf("sync rule %d: target %q is not a defined medium", n, r.To)
			}
			if r.From != "" {
				if _, ok := c.Media[r.From]; !ok {
					return fmt.Errorf("sync rule %d: source %q is not a defined medium", n, r.From)
				}
			}
		}
		from := r.From
		if from == "" {
			from = c.Landing
		}
		if from == r.To {
			return fmt.Errorf("sync rule %d: source and target are the same medium %q", n, r.To)
		}
		if r.Last < 0 {
			return fmt.Errorf("sync rule %d: `last` must not be negative", n)
		}
	}
	return nil
}

// validDrillTiers is the accepted set for the drill tier token (kept here so config
// validation needs no dependency on package drill, which depends on no config).
var validDrillTiers = map[string]bool{"": true, "sample": true, "checksum": true, "structural": true, "chain": true, "stock": true}

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
		return fmt.Errorf("drill: unknown tier %q (known: sample, checksum, structural, chain, stock)", d.Tier)
	}
	if d.From != "" && len(c.Media) > 0 {
		if _, ok := c.Media[d.From]; !ok {
			return fmt.Errorf("drill: source %q is not a defined medium", d.From)
		}
	}
	return nil
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
