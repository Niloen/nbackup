package restorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// errDestSetup marks a restore that failed before writing anything because the
// destination directory could not be created. A failure carrying it leaves no
// partial tree, so the caller reports it plainly rather than warning about an
// incomplete restore.
var errDestSetup = errors.New("destination could not be created")

// decode.go is the read-side scheme operation. The decoder reverses an archive's
// transforms: given a raw byte stream the ReadStore already opened, it composes
// the decode pipeline (decrypt → decompress → tar, each placed per the plan) into
// a sink (tar, hash, or list). The store supplies only the bytes; the scheme and
// the far-end tar live here, in the operation — as in a filesystem (cp/gzip
// compose the transform, the FS just reads/writes). Verify, restore, recover, and
// drill all share this one decode path.
type decoder struct {
	deps Deps
}

// decodePlan is the resolved recipe for reversing an archive's transforms on
// restore: the schemes and their invocation opts, plus where each step runs.
// decryptInSink is policy (see Restorer.planDecode) — true when the key is
// client-held and reached over `--to`, so decrypt runs on the target. remote
// notes a `--to` target, which is what makes fusing decompress with tar
// worthwhile (ship compressed bytes over the wire).
type decodePlan struct {
	compress      string
	compressOpts  compress.Options
	encrypt       string
	decryptOpts   crypt.Options
	decryptInSink bool
	remote        bool
	// atomSizes set = the archive is atomic: the raw stream is these per-atom
	// encrypted sizes back to back, and decode runs one decrypt child per atom
	// (see atomic.go). nil = a single-stream decode, exactly as before shapes.
	atomSizes []int64
}

// restoreArchive streams an archive (from a ReadStore) through the
// decode→extract pipeline into the destination (members nil = whole-archive
// listed-incremental restore; members set = selected-file recovery, no
// deletions). Decrypt lands in the sink or the local filters per
// plan.decryptInSink. Decompress fuses with tar only for a remote target — that
// is what ships compressed bytes over the wire; locally the split is free, and
// keeping decompress in the filters keeps the role-tagged error contract sharp
// (a Sink fault is a tar/composition fault, a Filters fault is a decode fault —
// what the drill's failure taxonomy classifies on).
func (d *decoder) restoreArchive(rc io.ReadCloser, plan decodePlan, archiverType, archName, dle string, dst dest, members []string) error {
	if plan.atomSizes != nil && plan.decryptInSink {
		rc.Close()
		return fmt.Errorf("an atomic archive decrypts per atom on the server; client-held keys are not supported on this path — restore without --to (or use the documented stock file-loop on the client)")
	}
	arch, err := d.deps.ArchiverFor(archiverType, archName, dle, dst.host)
	if err != nil {
		rc.Close()
		return err
	}
	// Only a directory destination is the generic layer's to create; an opaque one
	// (pipe) belongs to the archiver's restore command alone.
	if arch.DestIsDir() {
		if err := dst.exec.MkdirAll(dst.dir); err != nil {
			// The destination could not even be created (e.g. an unreachable `--to`
			// client): nothing was written, so mark it so the caller does not warn
			// about a "partial" restore that never started.
			rc.Close()
			return errors.Join(errDestSetup, err)
		}
	}
	src, filters, err := decodeSource(rc, plan)
	if err != nil {
		rc.Close()
		return err
	}
	sink := xfer.NewProgramSink(dst.exec).Add(filters.fused...).Add(arch.RestoreStage(dst.dir, members))
	_, err = xfer.Transfer(context.Background(), src, filters.local, sink)
	return err
}

// decodedFilters is a decode pipeline's placed remainder: the local filter chain and
// the stages fused into the sink (a remote target's decompress).
type decodedFilters struct {
	local xfer.Filters
	fused []programs.Cmd
}

// decodeSource turns an archive's raw stream into the transfer source plus the placed
// decode stages, per the plan's shape — the ONE place the stream/atomic split lives:
//
//   - single stream (nil atomSizes): the source is the raw bytes; decrypt and
//     decompress place as local filters or fuse into the sink per the plan.
//   - atomic: the source is the per-atom decrypt loop's plaintext (decrypt is
//     consumed here, server-side by construction); only decompress remains to place.
func decodeSource(rc io.ReadCloser, plan decodePlan) (xfer.Source, decodedFilters, error) {
	decrypt, decompress, err := buildDecode(plan.compress, plan.compressOpts, plan.encrypt, plan.decryptOpts)
	if err != nil {
		return nil, decodedFilters{}, err
	}
	if plan.atomSizes != nil {
		fused, local := xfer.SplitTransforms(xfer.Transform{Cmd: decompress, Fused: plan.remote})
		return xfer.Reader(atomicPlaintext(rc, plan.atomSizes, decrypt)), decodedFilters{local: local, fused: fused}, nil
	}
	fused, local := xfer.SplitTransforms(
		xfer.Transform{Cmd: decrypt, Fused: plan.decryptInSink},
		xfer.Transform{Cmd: decompress, Fused: plan.remote},
	)
	return xfer.Reader(rc), decodedFilters{local: local, fused: fused}, nil
}

// VerifyChecksum hashes an archive's raw stream (a ReadStore endpoint) and
// reports whether it matches the recorded sha — a transfer with no decode
// (source → Hash sink). A clean read whose hash differs returns (false, nil); a
// read fault returns (false, err). The Hash sink's part writer is a
// hash.Hash, whose Write never errors, so any transfer error tagged RoleCommit
// is necessarily the sink's own Commit-time verdict (the checksum comparison)
// — every other role (a mid-copy RoleSink included, since it can only mean the
// upstream read faulted while draining into that writer) is a genuine fault.
func (r *Restorer) VerifyChecksum(rc io.ReadCloser, sha string) (bool, error) {
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(rc), xfer.NewFilters(), xfer.Hash(sha))
	if terr != nil {
		var xe *xfer.Error
		if errors.As(terr, &xe) && xe.Role == xfer.RoleCommit {
			return false, nil // clean read, hash differs
		}
		return false, terr
	}
	return true, nil
}

// ListMembers decodes an archive's stream (a ReadStore endpoint, server-side
// filters) per its recorded shape and lists its members (`tar -t`) — the verify
// path's structural check. It resolves the shape's decode itself (an atomic
// archive runs the per-atom decrypt loop), so the caller passes the archive's
// record and never picks a variant. It returns the listed members and the raw,
// role-tagged transfer error for the caller to classify and hint.
//
// An archiver that cannot list (pipe's opaque stream) still gets the decode
// proof: the same pipeline drains to discard, so a broken key, scheme, or
// truncated stream surfaces exactly as for a listable archive — only the member
// comparison is off the table (nil members, which the caller must not diff).
func (r *Restorer) ListMembers(rc io.ReadCloser, a record.Archive, opts crypt.Options, arch archiver.Archiver) ([]record.Member, error) {
	plan := decodePlan{compress: a.Compress, compressOpts: r.deps.CompressOpts, encrypt: a.Encrypt, decryptOpts: opts}
	if a.Shape == record.ShapeAtomic {
		sizes, err := r.atomSizes(archiveio.Ref{Run: a.Run, DLE: a.DLE, Level: a.Level})
		if err != nil {
			rc.Close()
			return nil, err
		}
		plan.atomSizes = sizes
	}
	// A local list runs every transform server-side (nothing fuses with a far tar).
	src, filters, err := decodeSource(rc, plan)
	if err != nil {
		rc.Close()
		return nil, err
	}
	if !arch.CanList() {
		_, terr := xfer.Transfer(context.Background(), src, filters.local, xfer.Writer(io.Discard))
		return nil, terr
	}
	ls := &listSink{arch: arch}
	if _, terr := xfer.Transfer(context.Background(), src, filters.local, ls); terr != nil {
		// A faulted transfer may not have run Commit, so nothing synchronized with
		// the list goroutine — its members must not be read (and are meaningless
		// for a stream that did not decode cleanly anyway).
		return nil, terr
	}
	return ls.members, nil
}

// buildDecode returns the decrypt and decompress commands that reverse an
// archive's recorded transforms. The decrypt key reference is resolved per-DLE
// by the caller (decryptOptsFor — the one precedence rule). A none scheme
// yields an empty Cmd, which a transfer skips.
func buildDecode(compressScheme string, copts compress.Options, encrypt string, dopts crypt.Options) (decrypt, decompress programs.Cmd, err error) {
	ef, err := crypt.Filter(encrypt, dopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	cf, err := compress.Filter(compressScheme, copts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	return ef.Reverse, cf.Reverse, nil
}

// listSink consumes a decoded stream by listing its members (`tar -t`), keeping
// them on itself for the caller to read after the transfer. A bad stream
// (truncated decode, not-a-tar) fails the archiver's List; the members feed the
// comparison.
type listSink struct {
	arch    archiver.Archiver
	members []record.Member
	done    chan error
}

func (s *listSink) NextPart(ctx context.Context) (io.WriteCloser, int64, error) {
	pr, pw := io.Pipe()
	s.done = make(chan error, 1)
	go func() {
		members, err := s.arch.List(pr)
		s.members = members
		pr.CloseWithError(err)
		s.done <- err
	}()
	return pw, -1, nil
}

func (s *listSink) Commit(_ context.Context, _ xfer.SourceStats) error { return <-s.done }

// DecryptHint augments an extraction failure on an encrypted archive with the
// actionable cause restore-time decryption needs. gpg's raw "No secret key" is
// misleading for a symmetric (passphrase) dump — the real fix is to supply the
// passphrase the run had — so name both possibilities rather than leaving the
// operator with gpg's message alone. A nil error or a plaintext archive pass
// through.
func DecryptHint(scheme string, err error) error {
	if err == nil || scheme == "" || scheme == "none" {
		return err
	}
	return fmt.Errorf("%w\n(this archive is %s-encrypted, so extraction needs the key: for a passphrase/symmetric dump make sure an `encrypt:` block — config-wide or on this DLE's dumptype — points at the right passphrase_file; for a public-key dump ensure its private key is in the gpg keyring)", dropGpgAgentNoise(err), scheme)
}

// gpgAgentNoise is the stderr line gpg emits when its agent cannot open a
// pinentry on a tty-less run (cron, a pipe). It is pure environment noise — the
// actionable line ("decryption failed: No secret key") follows it — so surfacing
// it above NBackup's hint only misdirects the operator toward the agent.
const gpgAgentNoise = "gpg: problem with the agent: Inappropriate ioctl for device"

// dropGpgAgentNoise removes the agent-noise line from a decrypt failure's message
// while preserving the wrapped error chain (the drill and the CLI classify on
// errors.Is/As through it). Only that exact line is dropped; every other gpg
// stderr line survives.
func dropGpgAgentNoise(err error) error {
	if !strings.Contains(err.Error(), gpgAgentNoise) {
		return err
	}
	var kept []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.TrimSpace(line) == gpgAgentNoise {
			continue
		}
		kept = append(kept, line)
	}
	return &filteredErr{msg: strings.Join(kept, "\n"), cause: err}
}

// filteredErr rewords an error's message but keeps its chain intact for
// errors.Is/As classification.
type filteredErr struct {
	msg   string
	cause error
}

func (e *filteredErr) Error() string { return e.msg }
func (e *filteredErr) Unwrap() error { return e.cause }
