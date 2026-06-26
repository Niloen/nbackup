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

// transfer.go holds the engine's read-side transfer compositions: the operations (restore,
// recover, verify, drill) compose an xfer.Transfer from a clerk-provided medium Source through
// decode filters into a sink (tar, hash, or list). The clerk supplies only the byte endpoint;
// the codec and the far-end tar live here, in the operations — as in Amanda (amrestore
// decodes, the Recovery::Clerk only reads dumpfile bytes) and a filesystem (cp/gzip compose
// the transform, the FS just reads/writes).

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

// restoreArchive streams an archive (from a clerk Source) through the decode→extract pipeline
// into destDir on targetHost (members nil = whole-archive listed-incremental restore; members
// set = selected-file recovery, no deletions). Decrypt lands in the sink or the local filters
// per plan.DecryptInSink; decompress fuses with tar on the target so a remote restore ships
// compressed bytes.
func (e *Engine) restoreArchive(rc io.ReadCloser, plan DecodePlan, archiverType, destDir, targetHost string, members []string) error {
	target := e.Executor(targetHost)
	if err := target.MkdirAll(destDir); err != nil {
		return err
	}
	arch, err := e.restoreArchiver(archiverType, targetHost)
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

	sink := xfer.NewPrograms(target)
	if plan.DecryptInSink && encF.Reverse.Name != "" {
		sink.Add(encF.Reverse)
	}
	if compF.Reverse.Name != "" {
		sink.Add(compF.Reverse)
	}
	sink.Add(arch.RestoreStage(destDir, members))

	var filterCmds []programs.Cmd
	if !plan.DecryptInSink && encF.Reverse.Name != "" {
		filterCmds = append(filterCmds, encF.Reverse)
	}

	_, err = xfer.Transfer(xfer.Reader(rc), xfer.NewFilters(filterCmds...), sink, xfer.Opts{})
	return err
}

// verifyChecksum hashes an archive's raw stream (a clerk Source) and reports whether it matches
// the recorded sha — a transfer with no decode (source → Hash sink). A clean read whose hash
// differs returns (false, nil); a read fault returns (false, err).
func (e *Engine) verifyChecksum(rc io.ReadCloser, sha string) (bool, error) {
	_, terr := xfer.Transfer(xfer.Reader(rc), xfer.NewFilters(), xfer.Hash(sha), xfer.Opts{})
	if terr != nil {
		var xe *xfer.Error
		if errors.As(terr, &xe) && xe.Role == xfer.RoleSink {
			return false, nil // clean read, hash differs
		}
		return false, terr
	}
	return true, nil
}

// listMembers decodes an archive's stream (a clerk Source, server-side filters) and lists its
// members (`tar -t`) — the verify path's structural check. It returns the listed members and
// the raw, role-tagged transfer error for the caller to classify and hint.
func (e *Engine) listMembers(rc io.ReadCloser, codec, encrypt string, arch archiver.Archiver) ([]string, error) {
	decrypt, decompress, err := e.decodeFilters(codec, encrypt)
	if err != nil {
		return nil, err
	}
	res, terr := xfer.Transfer(xfer.Reader(rc), localDecode(decrypt, decompress), listSink{arch: arch}, xfer.Opts{})
	return res.SinkResult.Members, terr
}

// decodeFilters returns the decrypt and decompress commands that reverse an archive's recorded
// transforms, keyed by the engine's default decode options. A none scheme yields an empty Cmd,
// which a transfer skips.
func (e *Engine) decodeFilters(codec, encrypt string) (decrypt, decompress programs.Cmd, err error) {
	cf, err := compress.Filter(codec, e.fopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	ef, err := crypt.Filter(encrypt, e.dcopts)
	if err != nil {
		return programs.Cmd{}, programs.Cmd{}, err
	}
	return ef.Reverse, cf.Reverse, nil
}

// localDecode builds a local Filters chain (decrypt then decompress), skipping identities.
func localDecode(decrypt, decompress programs.Cmd) xfer.Filters {
	f := xfer.NewFilters()
	if decrypt.Name != "" {
		f = f.Add(decrypt)
	}
	if decompress.Name != "" {
		f = f.Add(decompress)
	}
	return f
}

// listSink consumes a decoded stream by listing its members (`tar -t`). A bad stream
// (truncated decode, not-a-tar) fails the archiver's List; the members feed the comparison.
type listSink struct{ arch archiver.Archiver }

func (s listSink) Drain(in io.Reader, _ func(int64)) (xfer.SinkResult, error) {
	members, err := s.arch.List(in)
	return xfer.SinkResult{Members: members}, err
}
