package engine

import (
	"errors"
	"io"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// decode.go is NBackup's read-side codec operation — Amanda's amrestore. A decoder reverses an
// archive's transforms: given a raw byte stream a clerk endpoint already opened, it composes the
// decode pipeline (decrypt → decompress → tar, each placed per the plan) into a sink (tar, hash,
// or list). The clerk supplies only the bytes; the codec and the far-end tar live here, in the
// operation — as in Amanda (amrestore decodes, the Recovery::Clerk only reads dumpfile bytes)
// and a filesystem (cp/gzip compose the transform, the FS just reads/writes).
//
// It depends on a narrow slice of the orchestrator — how to reach a host (exec), how to resolve
// the archiver that reverses a recorded type (archiverFor), and the decode option sets — not the
// whole engine, so verify, restore, recover, and drill all share one decode path.
type decoder struct {
	exec        func(host string) programs.Executor                    // executor for a target host (local or SSH)
	archiverFor func(typeName, host string) (archiver.Archiver, error) // archiver that reverses a recorded type
	fopts       compress.Options                                       // decompress invocation options
	dcopts      crypt.Options                                          // decrypt key reference / options
}

// newDecoder wires a decoder to the engine's host/archiver resolution and decode options.
func (e *Engine) newDecoder() *decoder {
	return &decoder{
		exec:        e.Executor,
		archiverFor: e.restoreArchiver,
		fopts:       e.fopts,
		dcopts:      e.dcopts,
	}
}

// DecodePlan is the engine-resolved recipe for reversing an archive's transforms on restore:
// the schemes and their invocation opts, plus where decrypt runs. DecryptInSink is resolved by
// the engine (policy) — true when the key is client-held and reached over `--to`, so decrypt
// runs on the target; otherwise it runs in the local server filters.
type DecodePlan struct {
	Codec         string
	CompressOpts  compress.Options
	Encrypt       string
	DecryptOpts   crypt.Options
	DecryptInSink bool
}

// restoreArchive streams an archive (from a clerk endpoint) through the decode→extract pipeline
// into destDir on targetHost (members nil = whole-archive listed-incremental restore; members
// set = selected-file recovery, no deletions). Decrypt lands in the sink or the local filters
// per plan.DecryptInSink; decompress fuses with tar on the target so a remote restore ships
// compressed bytes.
func (d *decoder) restoreArchive(rc io.ReadCloser, plan DecodePlan, archiverType, destDir, targetHost string, members []string) error {
	target := d.exec(targetHost)
	if err := target.MkdirAll(destDir); err != nil {
		return err
	}
	arch, err := d.archiverFor(archiverType, targetHost)
	if err != nil {
		return err
	}
	encF, err := crypt.Filter(plan.Encrypt, plan.DecryptOpts)
	if err != nil {
		return err
	}
	compF, err := compress.Filter(plan.Codec, plan.CompressOpts)
	if err != nil {
		return err
	}

	// Place each decode step: decrypt lands in the sink (on the target) when the key is
	// client-held, else in the local filters; decompress always fuses with tar on the target so
	// a remote restore ships compressed bytes. The sink chain is decrypt → decompress → tar.
	fused, filters := splitTransforms(
		transform{cmd: encF.Reverse, fused: plan.DecryptInSink},
		transform{cmd: compF.Reverse, fused: true},
	)
	sink := xfer.NewPrograms(target).Add(fused...).Add(arch.RestoreStage(destDir, members))

	_, err = xfer.Transfer(xfer.Reader(rc), filters, sink)
	return err
}

// verifyChecksum hashes an archive's raw stream (a clerk endpoint) and reports whether it
// matches the recorded sha — a transfer with no decode (source → Hash sink). A clean read whose
// hash differs returns (false, nil); a read fault returns (false, err).
func (d *decoder) verifyChecksum(rc io.ReadCloser, sha string) (bool, error) {
	_, terr := xfer.Transfer(xfer.Reader(rc), xfer.NewFilters(), xfer.Hash(sha))
	if terr != nil {
		var xe *xfer.Error
		if errors.As(terr, &xe) && xe.Role == xfer.RoleSink {
			return false, nil // clean read, hash differs
		}
		return false, terr
	}
	return true, nil
}

// listMembers decodes an archive's stream (a clerk endpoint, server-side filters) and lists its
// members (`tar -t`) — the verify path's structural check. It returns the listed members and
// the raw, role-tagged transfer error for the caller to classify and hint.
func (d *decoder) listMembers(rc io.ReadCloser, codec, encrypt string, arch archiver.Archiver) ([]string, error) {
	decrypt, decompress, err := d.decodeFilters(codec, encrypt)
	if err != nil {
		return nil, err
	}
	// A local list runs both transforms server-side (nothing fuses with a far tar).
	_, filters := splitTransforms(transform{cmd: decrypt}, transform{cmd: decompress})
	ls := &listSink{arch: arch}
	_, terr := xfer.Transfer(xfer.Reader(rc), filters, ls)
	return ls.members, terr
}

// decodeFilters returns the decrypt and decompress commands that reverse an archive's recorded
// transforms, keyed by the decoder's default decode options. A none scheme yields an empty Cmd,
// which a transfer skips.
func (d *decoder) decodeFilters(codec, encrypt string) (decrypt, decompress programs.Cmd, err error) {
	cf, err := compress.Filter(codec, d.fopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	ef, err := crypt.Filter(encrypt, d.dcopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	return ef.Reverse, cf.Reverse, nil
}

// listSink consumes a decoded stream by listing its members (`tar -t`), keeping them on
// itself for the caller to read after the transfer. A bad stream (truncated decode,
// not-a-tar) fails the archiver's List; the members feed the comparison.
type listSink struct {
	arch    archiver.Archiver
	members []string
}

func (s *listSink) Drain(in io.Reader) error {
	members, err := s.arch.List(in)
	s.members = members
	return err
}
