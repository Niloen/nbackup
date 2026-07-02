package restorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
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
func (d *decoder) restoreArchive(rc io.ReadCloser, plan decodePlan, archiverType string, dst dest, members []string) error {
	if err := dst.exec.MkdirAll(dst.dir); err != nil {
		// The destination could not even be created (e.g. an unreachable `--to`
		// client): nothing was written, so mark it so the caller does not warn
		// about a "partial" restore that never started.
		return errors.Join(errDestSetup, err)
	}
	arch, err := d.deps.ArchiverFor(archiverType, dst.host)
	if err != nil {
		return err
	}
	encF, err := crypt.Filter(plan.encrypt, plan.decryptOpts)
	if err != nil {
		return err
	}
	compF, err := compress.Filter(plan.compress, plan.compressOpts)
	if err != nil {
		return err
	}

	// Place each decode step; the sink chain is decrypt → decompress → tar.
	fused, filters := xfer.SplitTransforms(
		xfer.Transform{Cmd: encF.Reverse, Fused: plan.decryptInSink},
		xfer.Transform{Cmd: compF.Reverse, Fused: plan.remote},
	)
	sink := xfer.NewProgramChain(dst.exec).Add(fused...).Add(arch.RestoreStage(dst.dir, members))

	_, err = xfer.Transfer(context.Background(), xfer.Reader(rc), filters, sink)
	return err
}

// VerifyChecksum hashes an archive's raw stream (a ReadStore endpoint) and
// reports whether it matches the recorded sha — a transfer with no decode
// (source → Hash sink). A clean read whose hash differs returns (false, nil); a
// read fault returns (false, err).
func (r *Restorer) VerifyChecksum(rc io.ReadCloser, sha string) (bool, error) {
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(rc), xfer.NewFilters(), xfer.Hash(sha))
	if terr != nil {
		var xe *xfer.Error
		if errors.As(terr, &xe) && xe.Role == xfer.RoleSink {
			return false, nil // clean read, hash differs
		}
		return false, terr
	}
	return true, nil
}

// ListMembers decodes an archive's stream (a ReadStore endpoint, server-side
// filters) and lists its members (`tar -t`) — the verify path's structural
// check. It returns the listed members and the raw, role-tagged transfer error
// for the caller to classify and hint.
func (r *Restorer) ListMembers(rc io.ReadCloser, compressScheme, encrypt string, opts crypt.Options, arch archiver.Archiver) ([]string, error) {
	decrypt, decompress, err := r.decodeFilters(compressScheme, encrypt, opts)
	if err != nil {
		return nil, err
	}
	// A local list runs both transforms server-side (nothing fuses with a far tar).
	_, filters := xfer.SplitTransforms(xfer.Transform{Cmd: decrypt}, xfer.Transform{Cmd: decompress})
	ls := &listSink{arch: arch}
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(rc), filters, ls)
	return ls.members, terr
}

// decodeFilters returns the decrypt and decompress commands that reverse an
// archive's recorded transforms. The decrypt key reference (opts) is resolved
// per-DLE by the caller so a per-dumptype passphrase_file is honored, falling
// back to the config-wide default. A none scheme yields an empty Cmd, which a
// transfer skips.
func (r *Restorer) decodeFilters(compressScheme, encrypt string, opts crypt.Options) (decrypt, decompress programs.Cmd, err error) {
	cf, err := compress.Filter(compressScheme, r.deps.CompressOpts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	if opts == (crypt.Options{}) {
		opts = r.deps.DecryptOpts
	}
	ef, err := crypt.Filter(encrypt, opts)
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
	members []string
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
