// Package restorer is NBackup's read-side operation package — the mirror of the
// dumper. It executes what package recovery plans: Extract reconstructs a whole
// DLE as of a point in time (the deletion-accurate, listed-incremental chain
// restore behind `nb recover --all` — and behind a chain drill, which rehearses
// exactly this path), ExtractSelection extracts a browsed file selection, and
// OpenRecover builds the browse tree. It is written over the archive fs's read
// face (archivefs.ReadStore) plus narrow resolution funcs (Deps), never the
// engine, so it needs no real media to test.
//
// Failure classification rides on the errors it returns, never a side channel:
// archivefs.ErrMissingCopy / librarian.ErrVolumeUnavailable survive wrapping for
// errors.Is, and a role-tagged *xfer.Error surfaces for errors.As — a Sink fault
// is a tar/composition failure, anything else a decode/read failure. The drill
// depends on this contract.
package restorer

import (
	"errors"
	"fmt"
	"github.com/Niloen/nbackup/internal/archiveio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// Logf is the shared optional progress logger.
type Logf = logf.Logf

// Deps is what the restorer needs from the orchestrator: the archive fs's read
// face, the catalog's archive metadata (for chain/as-of planning), and the
// engine's resolution — hosts, archivers, per-DLE encryption posture. Funcs, not
// the engine, so the operations stay testable over fakes.
type Deps struct {
	Store    archivefs.ReadStore     // raw archive bytes + member lists
	Archives func() []record.Archive // catalog archive metadata, run-ordered
	Exec     func(host string) programs.Executor
	// ArchiverFor resolves the archiver that reverses a recorded type, built for
	// the host tar runs on ("" = server-side).
	ArchiverFor func(typeName, host string) (archiver.Archiver, error)
	// EncryptionFor resolves a DLE's configured encryption posture; ok is false
	// when the DLE is no longer in the config (an old run's DLE may be removed).
	EncryptionFor func(dleName string) (config.EncryptConfig, bool)
	// KnownHosts are the names a `--to` restore may target (hosts: + source hosts).
	KnownHosts   func() []string
	DisplayDLE   func(slug string) string // host:path identity for logging
	CompressOpts compress.Options         // decompress invocation options
	DecryptOpts  crypt.Options            // config-wide default decrypt key reference
}

// Restorer executes recovery plans. It is deliberately a concrete struct: the
// seam with alternative implementations is the ReadStore below it, and the
// sharpness up here is the Request type.
type Restorer struct {
	deps Deps
	dec  decoder
}

// New returns a Restorer over the orchestrator's deps.
func New(deps Deps) *Restorer {
	return &Restorer{deps: deps, dec: decoder{deps: deps}}
}

// Request is one whole-DLE reconstruction: which DLE, as of when, to where —
// the read-side mirror of archiver.BackupRequest. Host and Dest stay plain data
// because they are policy inputs: decrypt placement, the feasibility gate, and
// the empty-destination guard all branch on them.
type Request struct {
	DLE    string // catalog slug
	RunID  string // explicit target run (a drill pins one); or ""
	AsOf   string // "YYYY-MM-DD[ HH[:MM[:SS]]]", resolved to a run when RunID is ""
	Dest   string // destination directory (on Host)
	Host   string // "" = extract server-side; else a configured client (`--to`)
	Medium string // "" = any copy with fail-over; else pinned (`--from` / drill source)
	Force  bool   // allow a non-empty local Dest (skip the guard and the rollback)
}

// dest is a Request's destination resolved once: the executor extraction runs on
// and the directory there. If a second destination kind ever materializes (e.g.
// a --stdout tarball), this value — or an xfer.Sink — is what graduates into an
// abstraction; until then local-vs-remote is just the executor.
type dest struct {
	exec programs.Executor
	host string // "" = server-side
	dir  string
}

// Extract reconstructs a DLE as of a run (or an as-of date resolved to one) into
// the destination, replaying the restore chain with the archiver's
// listed-incremental extraction so each restored directory matches the archive's
// census — deletions are applied. That prunes the destination to match the
// backup, so unless Force is set a non-empty local destination is refused rather
// than silently destroyed; a remote (`--to`) destination is the operator's to
// honor. The chain is read in one ordered pass (consecutive same-volume archives
// reuse the mount), from any available copy or the pinned Medium.
func (r *Restorer) Extract(req Request, log Logf) error {
	runID := req.RunID
	if runID == "" {
		target, err := recovery.AsOf(r.deps.Archives(), req.AsOf)
		if err != nil {
			return err
		}
		runID = target
	}
	if req.Host != "" {
		if err := r.checkKnownHost(req.Host); err != nil {
			return err
		}
	}
	// When the guard ensured an empty local dest, a failed chain leaves only files
	// this restore wrote — so it can be rolled back to leave no half-restored tree.
	// With --force the dest held the operator's own content, so never auto-delete
	// it; a remote dest is never rolled back.
	rollbackOnFail := false
	if req.Host == "" && !req.Force {
		if err := errNonEmptyDest(req.Dest); err != nil {
			return err
		}
		rollbackOnFail = true
	}
	return r.extractChain(runID, req, rollbackOnFail, log)
}

// checkKnownHost validates a `--to` target up front: every non-localhost name
// would otherwise sail past and fail mid-restore with a raw SSH "exit status
// 255". A valid target appears under hosts: or as a configured source host.
func (r *Restorer) checkKnownHost(host string) error {
	names := append([]string(nil), r.deps.KnownHosts()...)
	for _, h := range names {
		if h == host {
			return nil
		}
	}
	sort.Strings(names)
	hint := "none configured"
	if len(names) > 0 {
		hint = strings.Join(names, ", ")
	}
	return fmt.Errorf("--to host %q is not a configured host (name it under hosts: or as a source host); known: %s", host, hint)
}

// extractChain replays a DLE's restore chain into the destination as one ordered
// one-pass read. Decrypt placement: a `--to` restore of a DLE that keeps its key
// on the client decrypts on that client — the only way to read an
// untrusted-server / client-symmetric archive (the server has no key). Every
// other restore decrypts server-side, which must be feasible (else fail fast);
// when decrypt is on the client there is nothing the server needs the key for,
// so the feasibility gate is skipped.
func (r *Restorer) extractChain(runID string, req Request, rollbackOnFail bool, log Logf) error {
	steps, err := recovery.Chain(r.deps.Archives(), req.DLE, runID)
	if err != nil {
		return r.friendlyDLEErr(req.DLE, err)
	}
	ec, _ := r.deps.EncryptionFor(req.DLE)
	decryptOnClient := req.Host != "" && ec.At == "client" && ec.SchemeName() != "none"
	if decryptOnClient {
		log.Log("decrypting on %s (encrypt.at: client) — only ciphertext leaves the server", req.Host)
	} else if err := r.ensureServerCanDecode(steps, log); err != nil {
		return err
	}

	stepByRef := make(map[archiveio.Ref]recovery.Step, len(steps))
	refs := make([]archiveio.Ref, 0, len(steps))
	for _, step := range steps {
		ref := archiveio.Ref{Run: step.RunID, DLE: step.DLE, Level: step.Level}
		stepByRef[ref] = step
		refs = append(refs, ref)
	}
	// A chain must be complete before anything is applied: a later incremental
	// extracted over a missing base would fabricate a wrong tree. Resolve
	// availability first (a no-op pass touches only the catalog, no media), so a
	// missing copy fails the restore before a single byte lands.
	if missing, err := r.deps.Store.ReadArchives(refs, req.Medium, func(archiveio.Ref, func() (io.ReadCloser, error)) error { return nil }); err != nil {
		return err
	} else if len(missing) > 0 {
		m := missing[0]
		return fmt.Errorf("%w: %s %s L%d has no copy%s — the chain cannot be replayed", archivefs.ErrMissingCopy, m.Run, m.DLE, m.Level, onMediumSuffix(req.Medium))
	}

	d := dest{exec: r.deps.Exec(req.Host), host: req.Host, dir: req.Dest}
	_, err = r.deps.Store.ReadArchives(refs, req.Medium, func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		step := stepByRef[ref]
		log.Log("extracting %s %s L%d -> %s", step.RunID, r.deps.DisplayDLE(step.DLE), step.Level, req.Dest)
		rc, oerr := open()
		if oerr != nil {
			return stepErr(step, DecryptHint(step.Encrypt, oerr))
		}
		plan := r.planDecode(step.DLE, step.Compress, step.Encrypt, req.Host)
		if xerr := r.dec.restoreArchive(rc, plan, step.Archiver, d, nil); xerr != nil {
			return stepErr(step, DecryptHint(step.Encrypt, xerr))
		}
		return nil
	})
	if err == nil {
		return nil
	}
	// The destination could not even be created — nothing landed, so report the
	// failure without the misleading "partial restore" warning (or a rollback of
	// a tree that was never written).
	if errors.Is(err, errDestSetup) {
		return err
	}
	// A chain that fails partway leaves an incomplete tree. If the dest was empty
	// when we started (no --force, local), every file in it is ours, so clear it —
	// a failed whole-DLE restore must not leave a half-restored tree a user could
	// mistake for complete. Otherwise warn loudly instead.
	if rollbackOnFail && req.Host == "" {
		if cerr := clearDirContents(req.Dest); cerr != nil {
			return fmt.Errorf("%w (and could not clean partial restore in %s: %v)", err, req.Dest, cerr)
		}
		return fmt.Errorf("%w — the chain is broken; %s was cleared (no partial tree left)", err, req.Dest)
	}
	return fmt.Errorf("%w — WARNING: %s now holds a PARTIAL, incomplete restore; discard it before use", err, req.Dest)
}

// stepErr wraps a step's failure with its identity, preserving the underlying
// error chain (sentinels and the role-tagged xfer error) for classification.
func stepErr(step recovery.Step, err error) error {
	return fmt.Errorf("extract %s %s L%d: %w", step.RunID, step.DLE, step.Level, err)
}

func onMediumSuffix(medium string) string {
	if medium == "" {
		return ""
	}
	return fmt.Sprintf(" on medium %q", medium)
}

// clearDirContents removes everything inside dir, leaving the (empty) dir itself.
// A dir that does not exist is already clean: a restore can fail before ever
// creating its destination, and complaining "could not clean" about a directory
// that was never written would bury the real error in noise.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// planDecode resolves the decode placement (policy): decrypt runs on the target
// (the sink) only when the key is client-held and reached over `--to`; otherwise
// on the local server. The decrypt key reference is decryptOptsFor's one rule.
func (r *Restorer) planDecode(dleName, compressScheme, encrypt, targetHost string) decodePlan {
	ec, _ := r.deps.EncryptionFor(dleName)
	inSink := ec.At == "client" && targetHost != ""
	return decodePlan{
		compress: compressScheme, compressOpts: r.deps.CompressOpts,
		encrypt: encrypt, decryptOpts: r.decryptOptsFor(dleName), decryptInSink: inSink,
		remote: targetHost != "",
	}
}

// ensureServerCanDecode fails fast when a restore would have the server decrypt
// an archive whose key cannot be there. Decode is server-side for both a plain
// restore and `--to` (only tar extraction moves to the client), so an archive
// encrypted on the client is the infeasible combination. The only case decidable
// for certain is a **client-side symmetric** dump (`encrypt.at: client`, a
// passphrase, no recipient): a symmetric key has no escrow path, so the
// passphrase stays on the client and the server provably cannot decrypt — a hard
// error pointing at the stock one-liner. A client-side **public-key** dump might
// have its private key escrowed in this server's keyring (a supported posture),
// so that is a warning, not a failure; a genuinely missing key still surfaces
// (with DecryptHint) when gpg runs. A DLE no longer in the config is skipped —
// we cannot know its posture.
func (r *Restorer) ensureServerCanDecode(steps []recovery.Step, log Logf) error {
	warned := map[string]bool{}
	for _, s := range steps {
		if s.Encrypt == "" || s.Encrypt == "none" {
			continue
		}
		ec, ok := r.deps.EncryptionFor(s.DLE)
		if !ok {
			continue
		}
		hardErr, warn := clientSideKeyRestore(ec, s.DLE)
		if hardErr != nil {
			return hardErr
		}
		if warn && !warned[s.DLE] {
			warned[s.DLE] = true
			log.Log("WARNING: DLE %s is encrypted on the client (encrypt.at: client); a server-side restore can only decrypt it if its private key is escrowed in this server's gpg keyring — otherwise restore it on the client", s.DLE)
		}
	}
	return nil
}

// clientSideKeyRestore classifies a server-side decode of an archive by the
// DLE's configured encryption posture (pure, so it is unit-tested without a live
// SSH dump): a hard error for client-side **symmetric** (a passphrase has no
// escrow path, so the server provably cannot decrypt), warn=true for client-side
// **public-key** (the private key may be escrowed on the server), or neither for
// server-side / plaintext.
func clientSideKeyRestore(ec config.EncryptConfig, dleName string) (hardErr error, warn bool) {
	if ec.At != "client" || ec.SchemeName() == "none" {
		return nil, false
	}
	if ec.Recipient == "" && ec.PassphraseFile != "" { // symmetric: no escrow path
		return fmt.Errorf("DLE %s is encrypted on the client with a passphrase (encrypt.at: client, symmetric): the passphrase never leaves the client, so a server-side restore cannot decrypt it — restore onto the client with `nb recover --all --to <client>:<path>` (decryption then runs there), or use the stock one-liner on the client", dleName), false
	}
	return nil, true
}

// friendlyDLEErr rewrites a chain error's message so the DLE reads as host:path
// — the form the user passed and the surrounding output uses — rather than the
// internal catalog slug the planning layer embeds, preserving the wrapped chain
// (the package contract: errors.Is/As survive). A DLE with no display mapping
// is left unchanged.
func (r *Restorer) friendlyDLEErr(dleName string, err error) error {
	disp := r.deps.DisplayDLE(dleName)
	if disp == dleName || err == nil {
		return err
	}
	return &filteredErr{msg: strings.ReplaceAll(err.Error(), `"`+dleName+`"`, `"`+disp+`"`), cause: err}
}

// DecryptOptsFor resolves the decrypt key reference for a DLE's archives — the
// exported face of decryptOptsFor for the engine's verify/drill paths.
func (r *Restorer) DecryptOptsFor(dleName string) crypt.Options {
	return r.decryptOptsFor(dleName)
}

// decryptOptsFor is the one place the decrypt key reference for a DLE's
// archives is decided: a DLE whose encrypt config sets a passphrase_file or
// program uses it (mirroring the dump side — without this a passphrase_file
// declared under a dumptype is silently dropped on read-back, so recover /
// verify --deep / drill cannot decrypt a config the README documents);
// otherwise — including a DLE no longer in the config — the config-wide
// default applies. Every decode path (planDecode, DecryptOptsFor) delegates
// here so the precedence cannot drift.
func (r *Restorer) decryptOptsFor(dleName string) crypt.Options {
	if ec, ok := r.deps.EncryptionFor(dleName); ok && (ec.PassphraseFile != "" || ec.Program != "") {
		return crypt.Options{Program: ec.Program, PassphraseFile: ec.PassphraseFile, Nice: r.deps.DecryptOpts.Nice}
	}
	return r.deps.DecryptOpts
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
