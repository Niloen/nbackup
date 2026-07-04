package restorer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// atomic.go is the FRAMED-ATOMIC read path: an atomic archive's parts are sealed
// atoms — each one complete encrypted message — so the decode loop runs ONE decrypt
// child per atom (gpg rejects concatenated messages by design) and concatenates the
// plaintexts, which are whole compressed frames and decode as a single stream. The
// atom boundaries come from the per-part seals: their sizes cut the raw stream, their
// cumulative RawSize is the member→atom map selective restore plans over.

// atomSizes returns an atomic archive's per-atom (encrypted, raw) sizes from its
// seals, or an error naming the remedy when no copy records them.
func (r *Restorer) atomSizes(ref archiveio.Ref) (enc, raw []int64, err error) {
	seals, err := r.deps.Store.AtomSeals(ref)
	if err != nil {
		return nil, nil, err
	}
	if len(seals) == 0 {
		return nil, nil, fmt.Errorf("atomic archive %s %s L%d records no per-part seals on any copy — its atoms cannot be cut for decode; run `nb rebuild`", ref.Run, ref.DLE, ref.Level)
	}
	for _, s := range seals {
		enc = append(enc, s.Size)
		raw = append(raw, s.RawSize)
	}
	return enc, raw, nil
}

// atomicPlaintext returns the decrypted — still compressed — stream of an atomic
// archive: rc is the raw concatenated atom stream, sizes cut it, and each atom runs
// through its own decrypt child (the read-side mirror of the per-atom seal on write).
// Closing the returned reader closes rc.
func atomicPlaintext(rc io.ReadCloser, sizes []int64, decrypt programs.Cmd) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReader(rc)
		for i, size := range sizes {
			if err := decodeAtom(io.LimitReader(br, size), decrypt, pw); err != nil {
				pw.CloseWithError(fmt.Errorf("decrypt atom %d of %d: %w", i+1, len(sizes), err))
				return
			}
		}
		pw.Close()
	}()
	return &closerPair{Reader: pr, a: pr, b: rc}
}

// decodeAtom runs one atom through its decrypt child (or straight through for a
// schemeless pipeline), writing the plaintext to w.
func decodeAtom(atom io.Reader, decrypt programs.Cmd, w io.Writer) error {
	if decrypt.Name == "" {
		_, err := io.Copy(w, atom)
		return err
	}
	out, wait, err := programs.Local().RunPipe(context.Background(), atom, decrypt)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(w, out)
	out.Close()
	if werr := wait(); cerr == nil {
		cerr = werr
	}
	return cerr
}

// closerPair is a reader whose Close closes both halves of a composed stream.
type closerPair struct {
	io.Reader
	a, b io.Closer
}

func (c *closerPair) Close() error {
	err := c.a.Close()
	if berr := c.b.Close(); err == nil {
		err = berr
	}
	return err
}

// restoreAtomic is restoreArchive's atomic sibling: per-atom decrypt (server-side —
// the loop respawns a child per atom, which cannot ride in a remote sink), then the
// usual decompress → tar chain (decompress fuses with a remote tar exactly as the
// stream shape's restore does).
func (d *decoder) restoreAtomic(rc io.ReadCloser, plan decodePlan, archiverType string, dst dest, members []string, sizes []int64) error {
	if plan.decryptInSink {
		rc.Close()
		return fmt.Errorf("an atomic archive decrypts per atom on the server; client-held keys are not supported on this path — restore without --to (or use the documented stock file-loop on the client)")
	}
	if err := dst.exec.MkdirAll(dst.dir); err != nil {
		rc.Close()
		return errors.Join(errDestSetup, err)
	}
	arch, err := d.deps.ArchiverFor(archiverType, dst.host)
	if err != nil {
		rc.Close()
		return err
	}
	decrypt, decompress, err := buildDecode(plan.compress, plan.compressOpts, plan.encrypt, plan.decryptOpts)
	if err != nil {
		rc.Close()
		return err
	}
	pt := atomicPlaintext(rc, sizes, decrypt)
	fused, filters := xfer.SplitTransforms(xfer.Transform{Cmd: decompress, Fused: plan.remote})
	sink := xfer.NewProgramSink(dst.exec).Add(fused...).Add(arch.RestoreStage(dst.dir, members))
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(pt), filters, sink)
	return terr
}

// ListMembersAtomic is ListMembers for an atomic archive: the structural verify's
// decode runs the per-atom decrypt loop, then decompress, then the archiver's list.
func (r *Restorer) ListMembersAtomic(rc io.ReadCloser, compressScheme, encrypt string, opts crypt.Options, arch archiver.Archiver, sizes []int64) ([]record.Member, error) {
	decrypt, decompress, err := buildDecode(compressScheme, r.deps.CompressOpts, encrypt, opts)
	if err != nil {
		rc.Close()
		return nil, err
	}
	pt := atomicPlaintext(rc, sizes, decrypt)
	_, filters := xfer.SplitTransforms(xfer.Transform{Cmd: decompress})
	ls := &listSink{arch: arch}
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(pt), filters, ls)
	return ls.members, terr
}

// extractAtomic is the atomic shape's selected-file recovery: selected members → raw
// extents (index order) → covering ATOMS via the seals' cumulative RawSize (the
// shape's frame table) → fetch those atoms (a ranged open where the medium can, whole
// consecutive parts by construction) → per-atom decrypt → one decompress child per
// group → discard to the member offsets → tar. handled=false falls back to the
// whole-stream atomic decode.
func (r *Restorer) extractAtomic(st recovery.ExtractStep, d dest, log Logf) (handled bool, err error) {
	ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
	encSizes, rawSizes, aerr := r.atomSizes(ref)
	if aerr != nil {
		return false, nil // seal-less copy: the whole-stream path reports the precise fault
	}
	idx, ierr := r.deps.Store.Index(ref)
	if ierr != nil || len(idx.Members) == 0 {
		return false, nil
	}
	extents, ok := planExtents(idx.Members, st.Members)
	if !ok {
		return false, nil
	}
	atoms := atomTable(encSizes, rawSizes)
	if atoms == nil {
		return false, nil // RawSize missing (e.g. a pre-atomic record): whole stream
	}
	groups := planGroups(atoms, extents)
	if len(groups) == 0 {
		return false, nil
	}
	first, perr := r.deps.Store.OpenRange(ref, "", groups[0].encOff, groups[0].encLen)
	if perr != nil {
		return false, nil // no ranged copy: the whole-stream atomic decode still works
	}

	decrypt, decompress, err := buildDecode(st.Compress, r.deps.CompressOpts, st.Encrypt, r.decryptOptsFor(st.DLE))
	if err != nil {
		first.Close()
		return true, err
	}
	arch, err := r.deps.ArchiverFor(st.Archiver, d.host)
	if err != nil {
		first.Close()
		return true, err
	}
	pr, pw := io.Pipe()
	go r.emitAtomGroups(ref, groups, atoms, encSizes, first, decrypt, decompress, pw)
	sink := xfer.NewProgramSink(d.exec).Add(arch.RestoreStage(d.dir, st.Members))
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(pr), xfer.NewFilters(), sink)
	return true, terr
}

// atomTable folds the seals' cumulative (raw, encrypted) sizes into a frame table —
// entry i is atom i's start offsets — or nil when RawSize was never recorded.
func atomTable(encSizes, rawSizes []int64) []record.Frame {
	table := make([]record.Frame, len(encSizes))
	var raw, enc int64
	for i := range encSizes {
		if rawSizes[i] <= 0 {
			return nil
		}
		table[i] = record.Frame{Raw: raw, Enc: enc}
		raw += rawSizes[i]
		enc += encSizes[i]
	}
	return table
}

// emitAtomGroups streams the planned groups into pw: each group's encoded range is a
// whole run of atoms (planGroups cuts at table boundaries), decrypted atom-by-atom and
// decompressed by one child per group, with the extents cut out of the plaintext
// exactly as the framed shape's emitGroup does.
func (r *Restorer) emitAtomGroups(ref archiveio.Ref, groups []rangeGroup, atoms []record.Frame, encSizes []int64, first io.ReadCloser, decrypt, decompress programs.Cmd, pw *io.PipeWriter) {
	for i, g := range groups {
		rc := first
		if i > 0 {
			var err error
			rc, err = r.deps.Store.OpenRange(ref, "", g.encOff, g.encLen)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		// The group's atoms: consecutive table entries starting at its encoded offset.
		sizes := groupAtomSizes(atoms, encSizes, g)
		pt := atomicPlaintext(rc, sizes, decrypt)
		err := emitGroup(g, pt, decompress, pw)
		pt.Close()
		if err != nil {
			pw.CloseWithError(fmt.Errorf("atomic ranged read of %s %s L%d: %w", ref.Run, ref.DLE, ref.Level, err))
			return
		}
	}
	if _, err := pw.Write(make([]byte, 1024)); err != nil {
		pw.CloseWithError(err)
		return
	}
	pw.Close()
}

// groupAtomSizes lists the encrypted sizes of the atoms a group's encoded range
// covers, in order — the cut list for its per-atom decrypt loop.
func groupAtomSizes(atoms []record.Frame, encSizes []int64, g rangeGroup) []int64 {
	var out []int64
	var covered int64
	for i, a := range atoms {
		if a.Enc < g.encOff {
			continue
		}
		if g.encLen >= 0 && covered >= g.encLen {
			break
		}
		out = append(out, encSizes[i])
		covered += encSizes[i]
	}
	return out
}

// SampleAtom is the drill sample tier's KEY-PROVING half on an atomic archive:
// decrypt-and-list ONE atom (rotated like the seal check's part) through the real
// pipeline, comparing the listed members+offsets against the index slice — the
// encrypted sibling of the framed shape's SampleFrame, proving the key opens this
// archive at one atom's egress.
func (r *Restorer) SampleAtom(ref archiveio.Ref, medium, compressScheme, encrypt string, opts crypt.Options, arch archiver.Archiver, rot int) (FrameSampleResult, error) {
	res := FrameSampleResult{}
	encSizes, rawSizes, err := r.atomSizes(ref)
	if err != nil {
		return res, nil // seal-less copy: nothing to sample against
	}
	atoms := atomTable(encSizes, rawSizes)
	if atoms == nil {
		return res, nil
	}
	idx, ierr := r.deps.Store.Index(ref)
	if ierr != nil || len(idx.Members) == 0 {
		return res, nil
	}
	decrypt, decompress, err := buildDecode(compressScheme, r.deps.CompressOpts, encrypt, opts)
	if err != nil {
		return res, err
	}
	decode := func(g rangeGroup, rc io.ReadCloser) io.ReadCloser {
		return atomicPlaintext(rc, groupAtomSizes(atoms, encSizes, g), decrypt)
	}
	return r.sampleTable(ref, medium, atoms, idx.Members, arch, rot, decode, decompress)
}
