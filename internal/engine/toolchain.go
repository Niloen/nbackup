package engine

import (
	"fmt"
	"path/filepath"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/dumper"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// toolchain is the engine's host/tool resolution service: it answers "which
// programs run where, with which options" — the executor a host's tools run on,
// the archiver for a (dumptype, host), and the per-dumptype transform schemes and
// options. It is pure config→runtime resolution (plus the archiver cache); it
// never touches media. Its peer is the depot, which resolves media the same way.
type toolchain struct {
	cfg            *config.Config
	archivers      map[string]archiver.Archiver // by cache key (dumptype or "@type", + host)
	compressScheme string                       // compression scheme for new archives
	fopts          compress.Options             // compress invocation options (level/threads/nice)
	dcopts         crypt.Options                // decrypt key reference for restore (from the default encrypt block)
}

func newToolchain(cfg *config.Config) *toolchain {
	return &toolchain{
		cfg:            cfg,
		archivers:      map[string]archiver.Archiver{},
		compressScheme: cfg.CompressScheme(),
		fopts: compress.Options{
			Program: cfg.Compress.Program,
			Level:   cfg.Compress.Level,
			Threads: cfg.Compress.Threads,
			Nice:    cfg.Nice,
		},
		// Decrypt options for restore come from the default encrypt block: the scheme to
		// reverse is recorded per-archive, but the key reference (passphrase file, binary
		// override) is supplied by the operator here; public-key schemes need none.
		dcopts: crypt.Options{
			Program:        cfg.Encrypt.Program,
			PassphraseFile: cfg.Encrypt.PassphraseFile,
			Nice:           cfg.Nice,
		},
	}
}

// checkCompress validates the default compression scheme's tools are runnable —
// the shared pre-flight the scheduler and the run lane both take as a closure.
func (t *toolchain) checkCompress() error { return compress.Check(t.compressScheme, t.fopts) }

// encryptionFor resolves the encryption scheme and encryptor options for a
// dumptype's dumps: the dumptype's own `encrypt` block, else the config default.
// The scheme is always a concrete name (gpg|none) — the exact peer of
// compressionFor — so a plaintext dump records "none", not "" (the two transforms
// describe their off-state identically in the artifact). crypt.Filter("none") is
// the identity, so the live pipeline treats it as no encryptor.
func (t *toolchain) encryptionFor(dtName string) (scheme string, opts crypt.Options) {
	ec := t.cfg.EncryptionFor(dtName)
	return ec.SchemeName(), crypt.Options{
		Program:        ec.Program,
		Recipient:      ec.Recipient,
		PassphraseFile: ec.PassphraseFile,
		Nice:           t.cfg.Nice,
	}
}

// compressionFor resolves the compression scheme and compressor options for a
// dumptype's dumps: the dumptype's own `compress` block, else the config default —
// the write-side peer of encryptionFor. The scheme is always a concrete name
// (zstd|gzip|none): it is recorded per-archive and reversed from the artifact, so
// it is never elided to "" — just as encryptionFor records a concrete "none".
func (t *toolchain) compressionFor(dtName string) (scheme string, opts compress.Options) {
	cc := t.cfg.CompressionFor(dtName)
	return cc.SchemeName(), compress.Options{
		Program: cc.Program,
		Level:   cc.Level,
		Threads: cc.Threads,
		Nice:    t.cfg.Nice,
	}
}

// dumptypeCompressSchemes returns the compression scheme each configured DLE's dumptype
// resolves to — the distinct set the server must be able to (de)compress. Used by `nb
// check` so a per-dumptype compress override is verified, not just the config default.
func (t *toolchain) dumptypeCompressSchemes() []string {
	var out []string
	for _, d := range t.cfg.DLEs() {
		out = append(out, t.cfg.CompressionFor(d.DumpTypeName()).SchemeName())
	}
	return out
}

// encodePlacement resolves a dumptype's write-side encode recipe from config — the
// per-dumptype compression and encryption scheme/opts and where each transform runs —
// for the producer (package dumper) to apply. The producer owns the tar source and the
// encode pipeline; this is only the config resolution.
func (t *toolchain) encodePlacement(dumpType string) dumper.EncodePlacement {
	compScheme, compOpts := t.compressionFor(dumpType)
	encScheme, encOpts := t.encryptionFor(dumpType)
	return dumper.EncodePlacement{
		CompressScheme: compScheme,
		CompressOpts:   compOpts,
		CompressClient: t.cfg.CompressionFor(dumpType).At == "client",
		EncryptScheme:  encScheme,
		EncryptOpts:    encOpts,
		EncryptClient:  t.cfg.EncryptionFor(dumpType).At == "client",
		AtomSize:       t.cfg.AtomSizeBytes(dumpType),
	}
}

// archiverFor resolves and caches the archiver for a (dumptype, host): the dumptype's
// named archiver definition (its type + options), with the executor for the DLE's host
// and that host's incremental-state root. A remote host yields an SSH executor (so tar
// runs on the client) and a client-side state root; a local/unlisted host yields the
// local executor and the server-side state root.
func (t *toolchain) archiverFor(dtName, host string) (archiver.Archiver, error) {
	dt := t.cfg.ResolveDumpType(dtName)
	def := t.cfg.ResolveArchiver(dt.Archiver)
	return t.openArchiver(dtName+"\x00"+host, def.Type, def.Options, host)
}

// openArchiver returns the cached archiver for key, or opens one of typeName for the host
// (with that host's executor, per-type option overrides, and incremental-state root) and
// caches it. It is the shared get-or-open the dump-side archiverFor and read-side
// restoreArchiver both use; they differ only in the cache key and whether a definition's
// options apply.
func (t *toolchain) openArchiver(key, typeName string, options map[string]string, host string) (archiver.Archiver, error) {
	if d, ok := t.archivers[key]; ok {
		return d, nil
	}
	ex := t.executorFor(host)
	opts := t.archiverOptions(typeName, options, host)
	// The host's state_dir is shared by every archiver on it; give this one a private
	// subtree named by its type (e.g. <state_dir>/gnutar) so two archivers can't collide.
	stateRoot := filepath.Join(t.cfg.StateDirFor(host), typeName)
	d, err := archiver.Open(typeName, opts, ex, stateRoot)
	if err != nil {
		return nil, err
	}
	t.archivers[key] = d
	return d, nil
}

// preflightDumptype validates one dumptype's pipeline tools before a dump: it resolves
// the archiver for (dumptype, host) — and runs its readiness Check when checkArchiver is
// set (the real dump does; plan validation only resolves) — and validates the dumptype's
// encryptor once. checked memoizes the per-dumptype encryption check across a plan's many
// DLEs. It is the shared pre-flight Run and ValidatePlan both run.
func (t *toolchain) preflightDumptype(dt, host string, checkArchiver bool, checked map[string]bool) error {
	arch, err := t.archiverFor(dt, host)
	if err != nil {
		return fmt.Errorf("dumptype %q: %w", dt, err)
	}
	if checkArchiver {
		if err := arch.Check(); err != nil {
			return err
		}
	}
	if !checked[dt] {
		scheme, opts := t.encryptionFor(dt)
		if err := crypt.Check(scheme, opts); err != nil {
			return err
		}
		checked[dt] = true
	}
	return nil
}

// restoreArchiver resolves and caches an archiver for reading, built with the
// executor for host: a remote DLE extracts on the client (tar runs there, the
// destination path is on the client — restore/recover land back where the data
// came from), a local/unlisted host ("") extracts server-side.
//
// The archive records its producing archiver's TYPE — how to reverse the stream —
// but a type's options live in a config definition (a pipe definition's
// restore_command is essential to extraction, where gnutar's flags are not). The
// DLE's dumptype is the config's mapping to that definition, so when the DLE is
// still configured and its dumptype still resolves to this type, restore runs
// with the definition's current options (the same config-is-source-of-truth rule
// decryptOptsFor applies to keys). A DLE no longer in the config — or remapped to
// another archiver type since the dump — falls back to the bare type with default
// options; an archiver whose options are load-bearing then errors, naming the
// missing option.
func (t *toolchain) restoreArchiver(typeName, dleName, host string) (archiver.Archiver, error) {
	for _, d := range t.cfg.DLEs() {
		if d.Name() != dleName {
			continue
		}
		if t.cfg.ResolveArchiver(t.cfg.ResolveDumpType(d.DumpTypeName()).Archiver).Type == typeName {
			return t.archiverFor(d.DumpTypeName(), host)
		}
		break
	}
	return t.openArchiver("@"+typeName+"\x00"+host, typeName, nil, host)
}

// executorFor returns the executor a DLE's host runs its tools on — the local machine for
// an empty or unlisted host, or an SSH executor for a host configured in the hosts: map.
// This is the one place "ssh" enters the engine; the archiver never learns it. Per-host
// state_dir and archiver-option overrides are resolved separately (see openArchiver), so
// a client's tar path and .snar root no longer ride on the executor.
func (t *toolchain) executorFor(host string) programs.Executor {
	hc, ok := t.cfg.RemoteHost(host)
	if !ok {
		return programs.Local()
	}
	return programs.SSH(programs.Params{
		User:         hc.User,
		Host:         host,
		Port:         hc.Port,
		IdentityFile: hc.IdentityFile,
		Options:      hc.Options,
	})
}

// probeReachable verifies a remote source host answers over SSH before a dump
// touches it, so an unreachable client surfaces as a transport error (as `nb check`
// reports it) rather than the misleading "GNU tar is required" the tar probe would
// emit when it runs over the dead connection. Local hosts are always reachable.
func (t *toolchain) probeReachable(host string) error {
	ssh, remote := t.cfg.RemoteHost(host)
	if !remote {
		return nil
	}
	ex := t.executorFor(host)
	if err := ex.Command("true").Run(); err != nil {
		return fmt.Errorf("source host %q unreachable over SSH (%s): %w — run `nb check` to diagnose", host, sshTarget(host, ssh), err)
	}
	return nil
}

// archiverOptions copies an archiver definition's options and merges this host's per-type
// overrides (`hosts.<host>.archivers.<typeName>`) over them — a client whose tar binary
// lives off the default PATH sets it there. The incremental-state root is not here: it is
// a host-level location passed to archiver.Open as stateRoot (see openArchiver).
func (t *toolchain) archiverOptions(typeName string, options map[string]string, host string) archiver.Options {
	opts := archiver.Options{}
	for k, v := range options {
		opts[k] = v
	}
	for k, v := range t.cfg.ArchiverOverrides(host, typeName) {
		opts[k] = v
	}
	return opts
}
