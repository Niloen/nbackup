package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/restore"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// restore.go is NBackup's restore/recover operation: it
// reconstructs a DLE from its backup chain — whole-DLE (deletion-accurate, listed-incremental),
// `--to` client restore, and file-level recover all run through one extraction path. It resolves
// the decode placement (engine policy: where the key may live) and feasibility, opens archives
// through the clerk, and decodes them through the decoder. Restore is config-policy-heavy — it
// reads encryption posture and host config per DLE — so it holds the config alongside the
// catalog, data path, and decoder, rather than a fan of narrow accessors.
type restorer struct {
	cat        *catalog.Catalog         // run list (chains, as-of resolution, browse)
	clerk      *clerk.Clerk             // byte endpoints + member index
	dec        *decoder                 // decode→extract pipeline
	cfg        *config.Config           // encryption posture + remote-host resolution per DLE
	fopts      compress.Options         // decompress invocation options
	dcopts     crypt.Options            // server-side decrypt key reference
	displayDLE func(slug string) string // host:path identity for logging
	// preferMedium forces reads to one copy (the `nb recover --from` medium); "" lets
	// the clerk pick any copy and fail over. Set per-call by the as-of entry points.
	preferMedium string
}

// newRestorer wires a restorer to the engine's catalog, data path, decoder, config, and opts.
func (e *Engine) newRestorer() *restorer {
	return &restorer{
		cat:        e.cat,
		clerk:      e.clerk,
		dec:        e.dec,
		cfg:        e.cfg,
		fopts:      e.fopts,
		dcopts:     e.dcopts,
		displayDLE: e.DisplayDLE,
	}
}

// Restore reconstructs a DLE as of a run into destDir. A whole-DLE restore
// replays the chain with GNU tar's listed-incremental extraction, which makes
// each restored directory match the archive's census — deleting anything on disk
// not in it. Pointed at a populated destDir that prunes unrelated files, so unless
// force is set Restore refuses a non-empty destination rather than silently
// destroying its contents. It reads from any available copy (own medium first).
func (r *restorer) Restore(runID, dleName, destDir string, force bool, logf Logf) error {
	if !force {
		if err := errNonEmptyDest(destDir); err != nil {
			return err
		}
	}
	// When the guard ensured an empty dest, a failed chain leaves only files this
	// restore wrote — so it can be rolled back to leave no half-restored tree. With
	// --force the dest held the operator's own content, so never auto-delete it.
	return r.restoreFrom(runID, dleName, destDir, "", !force, logf)
}

// RestoreTo restores a DLE onto a remote client:
// extraction runs on destHost over SSH and destPath is a path on that client, so the
// data lands back where it came from. Decode stays server-side, covering the server-side
// and asymmetric (private-key-on-server) postures; an untrusted-server client-only key
// restores with the documented stock one-liner. destHost must be configured under hosts:.
// The non-empty-destination guard is the operator's to honor here (the path is remote),
// so a whole-DLE restore over --to assumes an empty/new client directory.
func (r *restorer) RestoreTo(runID, dleName, destHost, destPath string, logf Logf) error {
	// Validate the target host up front: RemoteHost() treats every non-localhost name
	// as remote, so a typo'd host would otherwise sail past here and fail mid-restore
	// with a raw SSH "exit status 255". A valid target appears under hosts: or as a
	// configured source host.
	known := map[string]bool{}
	for h := range r.cfg.Hosts {
		known[h] = true
	}
	for _, d := range r.cfg.DLEs() {
		if d.Host != "" && d.Host != "localhost" {
			known[d.Host] = true
		}
	}
	if !known[destHost] {
		names := make([]string, 0, len(known))
		for h := range known {
			names = append(names, h)
		}
		sort.Strings(names)
		hint := "none configured"
		if len(names) > 0 {
			hint = strings.Join(names, ", ")
		}
		return fmt.Errorf("--to host %q is not a configured host (name it under hosts: or as a source host); known: %s", destHost, hint)
	}
	return r.restoreFrom(runID, dleName, destPath, destHost, false, logf)
}

// restoreFrom replays a DLE's restore chain into destDir. targetHost "" extracts
// server-side (the default); a `--to host:path` restore sets it so tar runs on that
// client and destDir is a client path — and, for a client-held key, decode runs there
// too. The exported Restore/RestoreTo are thin wrappers; reads fail over across copies
// (medium-scoped reads are the drill's own path, drillChain).
func (r *restorer) restoreFrom(runID, dleName, destDir, targetHost string, rollbackOnFail bool, logf Logf) error {
	steps, err := restore.Chain(r.cat.Archives(), dleName, runID)
	if err != nil {
		return r.friendlyDLEErr(dleName, err)
	}
	// Decrypt placement: a `--to` restore of a DLE that keeps its key on the client decrypts
	// on that client — the only way to read an untrusted-server / client-symmetric archive
	// (the server has no key). Every other restore decrypts server-side, which must be
	// feasible (else fail fast). When decrypt is on the client there is nothing the server
	// needs the key for, so the feasibility gate is skipped.
	ec := config.EncryptConfig{}
	if d, ok := r.dleByName(dleName); ok {
		ec = r.cfg.EncryptionFor(d.DumpTypeName())
	}
	decryptOnClient := targetHost != "" && ec.At == "client" && ec.SchemeName() != "none"
	if decryptOnClient {
		logf.Log("decrypting on %s (encrypt.at: client) — only ciphertext leaves the server", targetHost)
	} else if err := r.ensureServerCanDecode(steps, logf); err != nil {
		return err
	}
	for _, step := range steps {
		logf.Log("extracting %s %s L%d -> %s", step.RunID, r.displayDLE(step.DLE), step.Level, destDir)
		if err := r.extractStep(step, destDir, targetHost, ec); err != nil {
			wrapped := fmt.Errorf("extract %s %s L%d: %w", step.RunID, step.DLE, step.Level, err)
			// The destination could not even be created — nothing landed, so report the
			// failure without the misleading "partial restore" warning (or a rollback of
			// a tree that was never written).
			if errors.Is(err, errDestSetup) {
				return wrapped
			}
			// A chain that fails partway leaves an incomplete tree. If the dest was
			// empty when we started (no --force, local), every file in it is ours, so
			// clear it — a failed whole-DLE restore must not leave a half-restored
			// tree a user could mistake for complete. Otherwise warn loudly instead.
			if rollbackOnFail && targetHost == "" {
				if cerr := clearDirContents(destDir); cerr != nil {
					return fmt.Errorf("%w (and could not clean partial restore in %s: %v)", wrapped, destDir, cerr)
				}
				return fmt.Errorf("%w — the chain is broken; %s was cleared (no partial tree left)", wrapped, destDir)
			}
			return fmt.Errorf("%w — WARNING: %s now holds a PARTIAL, incomplete restore; discard it before use", wrapped, destDir)
		}
	}
	return nil
}

// clearDirContents removes everything inside dir, leaving the (empty) dir itself.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// extractStep replays one archive step into destDir as a decode→extract transfer whose
// zones each run where they should: decrypt where the key lives (the target for a
// client-held key reached over `--to`, otherwise the local server Filters) and decompress +
// tar extraction on the target Sink. The transfer crosses the wire at most once between
// zones — so a client-held key decrypts on the target (only ciphertext leaves the server),
// while a server-held key decrypts on the server and ships compressed plaintext (never
// inflated) to a remote target. targetHost "" extracts server-side.
func (r *restorer) extractStep(step restore.Step, destDir, targetHost string, ec config.EncryptConfig) error {
	return r.extractInto(step.RunID, step.DLE, step.Level, step.Compress, step.Encrypt, step.Archiver, destDir, targetHost, ec, nil)
}

// extractInto streams an archive's raw parts through the decode→extract pipeline into
// destDir on the target host, via the data path. With members it extracts only those
// entries in plain mode (selected-file recovery, which never deletes); without, a
// whole-archive listed-incremental chain restore. It is the engine's one extraction path —
// whole-DLE restore, `--to` client restore, and file-level recover all run through it; it
// adds the decrypt hint to whatever the data path surfaces.
func (r *restorer) extractInto(runID, dle string, level int, compressScheme, encrypt, archiverType, destDir, targetHost string, ec config.EncryptConfig, members []string) error {
	src, err := r.clerk.Open(clerk.Ref{Run: runID, DLE: dle, Level: level}, r.preferMedium)
	if err != nil {
		return decryptHint(encrypt, err)
	}
	plan := r.decodePlan(compressScheme, encrypt, ec, targetHost)
	return decryptHint(encrypt, r.dec.restoreArchive(src, plan, archiverType, destDir, targetHost, members))
}

// decodePlan resolves the decode placement (engine policy): decrypt runs on the target (the
// sink) only when the key is client-held and reached over `--to`; otherwise on the local
// server, with the server's default decrypt opts. The clerk just places the resolved plan.
func (r *restorer) decodePlan(compressScheme, encrypt string, ec config.EncryptConfig, targetHost string) DecodePlan {
	opts := r.dcopts
	// A per-dumptype encrypt block overrides the config-wide decrypt key reference, mirroring
	// the dump side — otherwise a passphrase_file declared under a dumptype is dropped on
	// read-back and a server-side restore cannot decrypt it.
	if ec.PassphraseFile != "" || ec.Program != "" {
		opts = crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile, Nice: r.dcopts.Nice}
	}
	inSink := ec.At == "client" && targetHost != ""
	return DecodePlan{Compress: compressScheme, CompressOpts: r.fopts, Encrypt: encrypt, DecryptOpts: opts, DecryptInSink: inSink}
}

// ensureServerCanDecode fails fast when a restore would have the server decrypt an
// archive whose key cannot be there. Decode is server-side for both a plain restore and
// `--to` (only tar extraction moves to the client), so an archive encrypted on the client
// is the infeasible combination. The only case the engine can decide for certain is a
// **client-side symmetric** dump (`encrypt.at: client`, a passphrase, no recipient): a
// symmetric key has no escrow path, so the passphrase stays on the client and the server
// provably cannot decrypt — that is a hard error pointing at the stock one-liner. A
// client-side **public-key** dump might have its private key escrowed in this server's
// keyring (a supported posture), so that is a warning, not a failure; a genuinely missing
// key still surfaces (with decryptHint) when gpg runs. A DLE no longer in the config is
// skipped — we cannot know its posture.
func (r *restorer) ensureServerCanDecode(steps []restore.Step, logf Logf) error {
	warned := map[string]bool{}
	for _, s := range steps {
		if s.Encrypt == "" || s.Encrypt == "none" {
			continue
		}
		d, ok := r.dleByName(s.DLE)
		if !ok {
			continue
		}
		hardErr, warn := clientSideKeyRestore(r.cfg.EncryptionFor(d.DumpTypeName()), s.DLE)
		if hardErr != nil {
			return hardErr
		}
		if warn && !warned[s.DLE] {
			warned[s.DLE] = true
			logf.Log("WARNING: DLE %s is encrypted on the client (encrypt.at: client); a server-side restore can only decrypt it if its private key is escrowed in this server's gpg keyring — otherwise restore it on the client", s.DLE)
		}
	}
	return nil
}

// clientSideKeyRestore classifies a server-side decode of an archive by the DLE's
// configured encryption posture (pure, so it is unit-tested without a live SSH dump): a
// hard error for client-side **symmetric** (a passphrase has no escrow path, so the server
// provably cannot decrypt), warn=true for client-side **public-key** (the private key may
// be escrowed on the server), or neither for server-side / plaintext.
func clientSideKeyRestore(ec config.EncryptConfig, dleName string) (hardErr error, warn bool) {
	if ec.At != "client" || ec.SchemeName() == "none" {
		return nil, false
	}
	if ec.Recipient == "" && ec.PassphraseFile != "" { // symmetric: no escrow path
		return fmt.Errorf("DLE %s is encrypted on the client with a passphrase (encrypt.at: client, symmetric): the passphrase never leaves the client, so a server-side restore cannot decrypt it — restore onto the client with `nb recover --all --to <client>:<path>` (decryption then runs there), or use the stock one-liner on the client", dleName), false
	}
	return nil, true
}

// dleByName returns the configured DLE with the given catalog name, if it is still in the
// config (an old run's DLE may have been removed).
// friendlyDLEErr rewrites a restore-chain error so the DLE reads as host:path — the form
// the user passed and the surrounding output uses — rather than the internal catalog slug
// the low-level restore package embeds. A DLE with no display mapping is left unchanged.
func (r *restorer) friendlyDLEErr(dleName string, err error) error {
	disp := r.displayDLE(dleName)
	if disp == dleName || err == nil {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), `"`+dleName+`"`, `"`+disp+`"`))
}

func (r *restorer) dleByName(name string) (config.DLE, bool) {
	for _, d := range r.cfg.DLEs() {
		if d.Name() == name {
			return d, true
		}
	}
	return config.DLE{}, false
}

// RestoreAsOf reconstructs a whole DLE as of a date (YYYY-MM-DD) into destDir —
// the deletion-accurate, whole-DLE counterpart to file-level recover. It resolves
// the date to the most recent run on or before it (the same resolution recover's
// browse uses), then replays that DLE's chain. So a bare date means the same run
// for both the browse view and a full restore.
func (r *restorer) RestoreAsOf(dle, asOf, destDir, from string, force bool, logf Logf) error {
	reset, err := r.useMedium(from)
	if err != nil {
		return err
	}
	defer reset()
	target, err := recovery.AsOf(r.cat.Archives(), asOf)
	if err != nil {
		return err
	}
	return r.Restore(target, dle, destDir, force, logf)
}

// RestoreAsOfTo is RestoreAsOf onto a remote client: it resolves the date to a run and
// restores the DLE's chain to destPath on destHost over SSH (see RestoreTo).
func (r *restorer) RestoreAsOfTo(dle, asOf, destHost, destPath, from string, logf Logf) error {
	reset, err := r.useMedium(from)
	if err != nil {
		return err
	}
	defer reset()
	target, err := recovery.AsOf(r.cat.Archives(), asOf)
	if err != nil {
		return err
	}
	return r.RestoreTo(target, dle, destHost, destPath, logf)
}

// useMedium pins reads to one copy for the duration of a restore (the `--from` medium),
// validating it is configured, and returns a reset to clear the pin. An empty from is
// the default (any copy, with fail-over).
func (r *restorer) useMedium(from string) (reset func(), err error) {
	if from != "" {
		if _, ok := r.cfg.Media[from]; !ok {
			return func() {}, fmt.Errorf("unknown medium %q (--from)", from)
		}
	}
	r.preferMedium = from
	return func() { r.preferMedium = "" }, nil
}

// errNonEmptyDest refuses a whole-DLE restore into a destination that already
// holds files. Listed-incremental extraction prunes the destination to match the
// backup (that is how deletions are applied), so restoring over an existing tree
// would delete unrelated files in it. A missing or empty directory is fine.
func errNonEmptyDest(destDir string) error {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // will be created empty
		}
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("destination %s is not empty: a whole-DLE restore prunes it to match the backup (GNU tar incremental extraction deletes files not in the archive), which would remove unrelated files — restore into a new/empty directory, or pass --force to restore into this one anyway", destDir)
	}
	return nil
}

// OpenRecover builds a browsable filesystem of a DLE as of a date (YYYY-MM-DD) —
// the recover entry point. Member lists are loaded lazily via the clerk (cache, or the
// on-medium index on a miss), so a fully-cached browse touches no media until extract.
func (r *restorer) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return recovery.BuildTree(r.cat.Archives(), dle, asOf, func(runID string, level int) ([]string, error) {
		return r.clerk.Members(clerk.Ref{Run: runID, DLE: dle, Level: level})
	})
}

// ExtractSelection extracts a selected set of files, grouped by their source
// archive, into destDir. It returns the number of member entries extracted.
func (r *restorer) ExtractSelection(steps []recovery.ExtractStep, destDir string, logf Logf) (int, error) {
	// File-level recover decodes server-side, so a client-only key is infeasible here —
	// fail fast (browse stays keyless; only extraction needs the key).
	for _, st := range steps {
		if d, ok := r.dleByName(st.DLE); ok {
			if hardErr, _ := clientSideKeyRestore(r.cfg.EncryptionFor(d.DumpTypeName()), st.DLE); hardErr != nil {
				return 0, hardErr
			}
		}
	}
	// Open the selected archives as one ordered, one-pass read (the clerk resolves and mounts;
	// the librarian keeps a volume loaded across consecutive same-volume reads), then extract
	// each. Recover always extracts server-side, so decrypt stays on the server.
	stepByRef := make(map[clerk.Ref]recovery.ExtractStep, len(steps))
	refs := make([]clerk.Ref, 0, len(steps))
	for _, st := range steps {
		ref := clerk.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
		stepByRef[ref] = st
		refs = append(refs, ref)
	}

	files := 0
	missing, err := r.clerk.ReadArchives(refs, "", func(ref clerk.Ref, open func() (io.ReadCloser, error)) error {
		st := stepByRef[ref]
		// An archive in the chain that holds none of the selected files contributes
		// nothing — skip it silently rather than logging a noisy "extracting 0 file(s)".
		if countFiles(st.Members) == 0 {
			return nil
		}
		logf.Log("extracting %d file(s) from %s %s L%d", countFiles(st.Members), st.RunID, r.displayDLE(st.DLE), st.Level)
		rc, serr := open()
		if serr != nil {
			return serr
		}
		// Resolve the per-dumptype encrypt block so a per-dumptype passphrase_file is honored on
		// file-level recovery (server-side decode), not just the config-wide one.
		var ec config.EncryptConfig
		if d, ok := r.dleByName(st.DLE); ok {
			ec = r.cfg.EncryptionFor(d.DumpTypeName())
		}
		plan := r.decodePlan(st.Compress, st.Encrypt, ec, "")
		if err := decryptHint(st.Encrypt, r.dec.restoreArchive(rc, plan, st.Archiver, destDir, "", st.Members)); err != nil {
			return err
		}
		files += countFiles(st.Members)
		return nil
	})
	if err != nil {
		return files, fmt.Errorf("recover: %w", err)
	}
	if len(missing) > 0 {
		return files, fmt.Errorf("recover: one or more selected archives have no available copy")
	}
	return files, nil
}

// countFiles counts the file members in a selection, excluding the parent
// directories the extractor recreates to hold them (the archiver-neutral member
// convention marks directories with a trailing slash). So recovering one nested
// file reports 1, not "2 entries" once its parent dir is counted.
func countFiles(members []string) int {
	n := 0
	for _, m := range members {
		if !strings.HasSuffix(m, "/") {
			n++
		}
	}
	return n
}
