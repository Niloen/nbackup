package restorer

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// ranged.go is selected-file recovery's ranged read path: instead of streaming a whole
// archive to extract a few members, it maps the selected members to raw-stream extents
// (the index's stream-order invariant: member i's extent ends at member i+1's offset),
// finds the covering decode frames, fetches only those encoded ranges off the medium
// (cloud ranged GETs, disk seeks), decodes each range with a fresh child (every frame
// boundary is a decode restart), and feeds tar exactly the wanted extents plus a 1 KiB
// NUL end-of-archive marker for a clean exit. Every missing ingredient — no index, no
// offsets, no frame table (unless the pipeline is the identity), no range-capable
// copy — falls back to the whole-stream path, which is the unchanged existing code.

// A raw extent (one selected member's byte range in the raw archive stream) and an
// encoded fetch are both media.Range values — the read ladder's one range vocabulary;
// an open end (the last member's extent, a fetch running to the stream's tail) is the
// range's own Len <= 0 form.

// rangeGroup is one coalesced fetch: the encoded range to read off the medium, the raw
// offset its decoded output begins at, and the raw extents to emit from within it,
// ascending.
type rangeGroup struct {
	enc      media.Range
	rawStart int64
	extents  []media.Range
}

// planExtents resolves the selected member paths to their raw extents via the index's
// stream-ordered member list, coalescing adjacent extents. ok is false when any
// selected member is absent from the index or lacks an offset — the fallback cue.
func planExtents(members []record.Member, selected []string) ([]media.Range, bool) {
	pos := make(map[string]int, len(members))
	for i, m := range members {
		pos[m.Path] = i
	}
	extents := make([]media.Range, 0, len(selected))
	for _, path := range selected {
		i, found := pos[path]
		if !found || members[i].Off < 0 {
			return nil, false
		}
		e := media.Range{Off: members[i].Off} // open-ended: the last member runs to the stream's end
		if i+1 < len(members) {
			if members[i+1].Off < 0 {
				return nil, false
			}
			e.Len = members[i+1].Off - e.Off
		}
		extents = append(extents, e)
	}
	sort.Slice(extents, func(i, j int) bool { return extents[i].Off < extents[j].Off })
	// Coalesce adjacent/overlapping extents (consecutive members selected together).
	out := extents[:0]
	for _, e := range extents {
		if n := len(out); n > 0 && (out[n-1].End() < 0 || out[n-1].End() >= e.Off) {
			prev := &out[n-1]
			if prev.End() >= 0 {
				if e.End() < 0 {
					prev.Len = 0 // runs to the stream's end
				} else if e.End() > prev.End() {
					prev.Len = e.End() - prev.Off
				}
			}
			continue
		}
		out = append(out, e)
	}
	return out, true
}

// planGroups maps raw extents to coalesced encoded fetch groups. With a frame table
// each extent is widened to its covering frames; frames == nil is the identity
// pipeline (no transform children), where encoded and raw offsets coincide and each
// extent maps 1:1. Overlapping/adjacent frame windows merge into one fetch.
func planGroups(frames []record.Frame, extents []media.Range) []rangeGroup {
	if frames == nil {
		out := make([]rangeGroup, 0, len(extents))
		for _, e := range extents {
			out = append(out, rangeGroup{enc: e, rawStart: e.Off, extents: []media.Range{e}})
		}
		return out
	}
	var out []rangeGroup
	for _, e := range extents {
		// f: the last frame starting at or before the extent; g: the first frame at or
		// past its end (len(frames) = the stream's end when the extent runs into the tail).
		f := sort.Search(len(frames), func(i int) bool { return frames[i].Raw > e.Off }) - 1
		if f < 0 {
			f = 0 // frame 0 is {0,0}; a start before it cannot happen, but stay in range
		}
		g := len(frames)
		if e.End() >= 0 {
			g = sort.Search(len(frames), func(i int) bool { return frames[i].Raw >= e.End() })
		}
		encEnd := int64(-1)
		if g < len(frames) {
			encEnd = frames[g].Enc
		}
		if n := len(out); n > 0 && (out[n-1].enc.End() < 0 || out[n-1].enc.End() >= frames[f].Enc) {
			// Overlaps/adjoins the previous group's frame window: merge the fetches.
			prev := &out[n-1]
			if prev.enc.End() >= 0 {
				if encEnd < 0 {
					prev.enc.Len = 0 // runs to the stream's end
				} else if encEnd > prev.enc.End() {
					prev.enc.Len = encEnd - prev.enc.Off
				}
			}
			prev.extents = append(prev.extents, e)
			continue
		}
		enc := media.Range{Off: frames[f].Enc} // open-ended when the span runs to the tail
		if encEnd >= 0 {
			enc.Len = encEnd - enc.Off
		}
		out = append(out, rangeGroup{enc: enc, rawStart: frames[f].Raw, extents: []media.Range{e}})
	}
	return out
}

// selectionPlan is a ranged selection's shape-resolved ingredients: the restart table
// (the index's frame table; the atoms' cumulative sizes; nil entries impossible), an
// optional per-group stream decoration (the atomic per-atom decrypt loop), and the
// decompress child. One resolver, so the ranged path itself never branches on shape.
type selectionPlan struct {
	table      []record.Frame
	decode     func(rangeGroup, io.ReadCloser) io.ReadCloser // nil = the fetched range is already the decode input
	decompress programs.Cmd
}

// planSelection resolves a step's selectionPlan per its recorded shape, or ok=false
// when an ingredient is missing — the fallback cue (the whole-stream path then reports
// any real fault precisely).
func (r *Restorer) planSelection(st recovery.ExtractStep, idx record.Index) (selectionPlan, bool) {
	ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
	if st.Shape == record.ShapeAtomic {
		seals, err := r.deps.Store.AtomSeals(ref)
		if err != nil {
			return selectionPlan{}, false
		}
		table := st.Shape.RestartTable(nil, seals)
		if table == nil {
			return selectionPlan{}, false // no seals, or RawSize never recorded: whole stream
		}
		decrypt, decompress, err := buildDecode(st.Compress, r.deps.CompressOpts, st.Encrypt, r.decryptOptsFor(st.DLE))
		if err != nil {
			return selectionPlan{}, false
		}
		encSizes := sealSizes(seals)
		return selectionPlan{
			table: table,
			decode: func(g rangeGroup, rc io.ReadCloser) io.ReadCloser {
				return atomicPlaintext(rc, groupAtomSizes(table, encSizes, g), decrypt)
			},
			decompress: decompress,
		}, true
	}
	identity := (st.Compress == "" || st.Compress == "none") && (st.Encrypt == "" || st.Encrypt == "none")
	table := st.Shape.RestartTable(idx.Frames, nil)
	if table == nil && !identity {
		return selectionPlan{}, false // no restart points and a real transform: whole stream only
	}
	var decompress programs.Cmd
	if !identity {
		cf, err := compress.Filter(st.Compress, r.deps.CompressOpts)
		if err != nil {
			return selectionPlan{}, false
		}
		decompress = cf.Reverse
	}
	return selectionPlan{table: table, decompress: decompress}, true
}

// rangedGroups resolves a step's selection to its coalesced ranged-fetch groups
// (selected members → raw extents → covering frames or atoms) without moving any of the
// medium's payload — the plan the extractor executes and the cost estimate prices, so a
// forecast matches the read it forecasts. ok is false at exactly extractSelected's
// paper-only fallback gates: no member index, an unlisted or offset-less member, a shape
// whose restart table is absent under a real transform, or an archiver whose streams do
// not splice. The one gate that moves a byte — the live OpenRange capability probe —
// stays in extractSelected.
func (r *Restorer) rangedGroups(st recovery.ExtractStep) ([]rangeGroup, selectionPlan, string, bool) {
	ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
	idx, ierr := r.deps.Store.Index(ref)
	if ierr != nil || len(idx.Members) == 0 {
		return nil, selectionPlan{}, "no member index (pre-shapes archive)", false
	}
	extents, ok := planExtents(idx.Members, st.Members)
	if !ok {
		return nil, selectionPlan{}, "a selected member has no recorded stream offset", false
	}
	plan, ok := r.planSelection(st, idx)
	if !ok {
		return nil, selectionPlan{}, "not seekable — no restart table for this shape", false
	}
	groups := planGroups(plan.table, extents)
	if len(groups) == 0 {
		return nil, selectionPlan{}, "no fetch groups planned", false
	}
	// The archiver must DECLARE its streams spliceable (member extents are
	// independently restorable and a trailer terminates an assembled stream) — reporting
	// offsets alone is not that promise. Splicing is a format property, so it resolves
	// server-side ("") for both the estimate and the extract.
	arch, err := r.deps.ArchiverFor(st.Archiver, st.ArchiverName, st.DLE, "")
	if err != nil || arch.SpliceTrailer() == nil {
		return nil, selectionPlan{}, "archiver cannot splice member ranges", false
	}
	return groups, plan, "", true
}

// groupsEgress totals the encoded bytes the ranged fetch groups pull off the medium:
// each bounded group its enc.Len, each open-ended group (running to the stream's tail)
// the bytes from its start to encodedSize. It is the egress a ranged selection spends —
// what the estimate quotes and the extract log reports. encodedSize <= a group's start
// (an unknown archive size) leaves that group's tail uncounted rather than guessing.
func groupsEgress(groups []rangeGroup, encodedSize int64) int64 {
	var n int64
	for _, g := range groups {
		switch {
		case g.enc.Len > 0:
			n += g.enc.Len
		case encodedSize > g.enc.Off:
			n += encodedSize - g.enc.Off
		}
	}
	return n
}

// extractSelected attempts the ranged extraction of one step's selection, per the
// archive's shape: selected members → raw extents → covering frames (or atoms) →
// coalesced ranged fetches → decode → tar. handled is false — with no error — when an
// ingredient is missing and the caller should fall back to the whole-stream path; once
// the first range is open (the capability probe), a failure is a real extraction error.
// egress is the encoded bytes the ranged read pulled (for the caller's read log), valid
// only when handled and err is nil.
func (r *Restorer) extractSelected(st recovery.ExtractStep, d dest, prog ReadProgress, label string) (handled bool, egress int64, err error) {
	groups, plan, _, ok := r.rangedGroups(st)
	if !ok {
		return false, 0, nil
	}
	ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
	// The archiver reverses on the destination host; its splice trailer (a format
	// property, so identical to rangedGroups' server-side resolve) terminates the stream.
	arch, err := r.deps.ArchiverFor(st.Archiver, st.ArchiverName, st.DLE, d.host)
	if err != nil {
		return true, 0, err
	}
	trailer := arch.SpliceTrailer()
	if trailer == nil {
		return false, 0, nil
	}
	// Capability probe: open the first group's range now. Any failure here — no seals,
	// a range-incapable medium, no copy — is the fallback cue, before any byte moved.
	first, perr := r.deps.Store.OpenRange(ref, "", groups[0].enc)
	if perr != nil {
		return false, 0, nil
	}

	egress = groupsEgress(groups, r.encodedSize(ref))
	prog.Reading("RANGED", label, egress)
	pr, pw := io.Pipe()
	go r.emitGroups(ref, groups, first, plan, trailer, pw, prog)

	sink := xfer.NewProgramSink(d.exec).Add(arch.RestoreStage(d.dir, st.Members))
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(pr), xfer.NewFilters(), sink)
	return true, egress, terr
}

// ArchiveRead is how one archive of a file selection will be read: the encoded bytes
// pulled off the medium and in how many fetches, whether that is a ranged read (only the
// selected members' covering frames) or the whole archive, and — for the extraction plan
// — the whole-archive size it is a fraction of, the file count, and (on a whole read) the
// reason ranging was not possible. It lets a cost estimate price the real egress instead
// of the whole payload, and an EXPLAIN-style plan show the read strategy per archive.
type ArchiveRead struct {
	Ref    archiveio.Ref
	Bytes  int64 // encoded bytes pulled off the medium (a ranged read's covering frames, else the whole archive)
	Parts  int64 // fetches: ranged groups, else the whole archive's part count
	Ranged bool
	Whole  int64  // the whole archive's on-medium size (what Bytes is a fraction of on a ranged read)
	Files  int    // selected file entries in this archive
	Reason string // on a whole read, why ranging was not possible (empty on a ranged read)
}

// SelectionReads plans, without moving payload, how each step's selection will be read —
// the ranged-aware core of the recovery cost estimate. It plans through rangedGroups
// (extractSelected's own paper-only gates), so the forecast matches the extract: a
// rangeable archive reports only its covering-frame egress, everything else the whole
// compressed archive. It assumes the extract's live capability probe succeeds — true
// whenever a framed/atomic archive's placement carries the per-part seals written
// alongside its frames, i.e. the normal case; a placement missing them would read whole
// and the estimate would under-quote. Steps holding no files (directory-only) read
// nothing and are omitted, matching ExtractSelection.
func (r *Restorer) SelectionReads(steps []recovery.ExtractStep) []ArchiveRead {
	sizes := r.encodedSizes()
	out := make([]ArchiveRead, 0, len(steps))
	for _, st := range steps {
		files := countFilePaths(st.Members)
		if files == 0 {
			continue
		}
		ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
		size := sizes[ref]
		rd := ArchiveRead{Ref: ref, Bytes: size.bytes, Parts: size.parts, Whole: size.bytes, Files: files}
		switch groups, _, reason, ok := r.rangedGroups(st); {
		case !ok:
			rd.Reason = reason
		case r.deps.RangedCopy != nil && !r.deps.RangedCopy(ref):
			// The format could range, but no copy's medium can serve one (tape
			// streams): the extract will fall back to the whole stream, so the
			// plan must promise — and price — exactly that.
			rd.Reason = "no copy on a range-capable medium"
		default:
			rd.Bytes = groupsEgress(groups, size.bytes)
			rd.Parts = int64(len(groups))
			rd.Ranged = true
		}
		out = append(out, rd)
	}
	return out
}

// archiveSize is an archive's whole-read cost: its on-medium compressed size and the
// number of part-fetches reading it takes.
type archiveSize struct {
	bytes int64
	parts int64
}

// encodedSizes indexes the catalog's archive metadata by ref for the whole-archive
// figures a read estimate and read log need (an open-ended group's tail, the fallback
// size). Built once per estimate; a lone lookup uses encodedSize.
func (r *Restorer) encodedSizes() map[archiveio.Ref]archiveSize {
	m := map[archiveio.Ref]archiveSize{}
	for _, a := range r.deps.Archives() {
		parts := int64(1)
		if a.Parts > 1 {
			parts = int64(a.Parts)
		}
		m[archiveio.Ref{Run: a.Run, DLE: a.DLE, Level: a.Level}] = archiveSize{bytes: a.Compressed, parts: parts}
	}
	return m
}

// encodedSize is one archive's on-medium compressed size from the catalog metadata, or 0
// when the ref is unknown — it bounds an open-ended ranged fetch and names the
// whole-archive size in the read log.
func (r *Restorer) encodedSize(ref archiveio.Ref) int64 {
	for _, a := range r.deps.Archives() {
		if a.Run == ref.Run && a.DLE == ref.DLE && a.Level == ref.Level {
			return a.Compressed
		}
	}
	return 0
}

// emitGroups streams the planned groups into pw: per group it fetches the encoded
// range (the first is already open — the probe), decodes it with a fresh child when
// the pipeline has one, discards up to each extent, emits the extent, and drains the
// group's tail so the decode child exits cleanly at its input's end. A trailing 1 KiB
// of NULs is tar's end-of-archive marker (PoC-proven clean exit).
func (r *Restorer) emitGroups(ref archiveio.Ref, groups []rangeGroup, first io.ReadCloser, plan selectionPlan, trailer []byte, pw *io.PipeWriter, prog ReadProgress) {
	for i, g := range groups {
		rc := first
		if i > 0 {
			var err error
			rc, err = r.deps.Store.OpenRange(ref, "", g.enc)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		rc = &countReadCloser{ReadCloser: rc, prog: prog} // count the encoded bytes pulled off the medium
		stream := rc
		if plan.decode != nil {
			stream = plan.decode(g, rc)
		}
		if err := emitGroup(g, stream, plan.decompress, pw); err != nil {
			stream.Close()
			pw.CloseWithError(fmt.Errorf("ranged read of %s %s L%d: %w", ref.Run, ref.DLE, ref.Level, err))
			return
		}
		stream.Close()
	}
	// The archiver's splice trailer terminates the assembled stream cleanly.
	if _, err := pw.Write(trailer); err != nil {
		pw.CloseWithError(err)
		return
	}
	pw.Close()
}

// FrameSampleResult is one structural sample's outcome (see Sample).
type FrameSampleResult struct {
	Ran    bool   // false: an ingredient was missing (no frames/offsets/ranged copy) — not a verdict
	OK     bool   // the listed members+offsets matched the index slice
	Detail string // the first mismatch, when !OK
	Unit   string // what was sampled: "frame" (framed shape) or "atom" (atomic — the key-proving check)
	Frame  int    // the sampled frame's/atom's index
	Bytes  int64  // encoded bytes fetched (the sample's egress); -1 = a to-the-end fetch of unknown size
}

// Sample structurally proves ONE restart unit of an archive at bounded egress — the
// drill sample tier's structural half, resolved per the recorded shape so the caller
// never picks a variant:
//
//   - framed: fetch the frames covering the members starting inside frame
//     (rot % frames), decode with a fresh child, `tar -tR`, and compare names AND
//     offsets (relative to the span's start) against the index slice.
//   - atomic: the same over ONE atom (the seals' cumulative sizes are the table) with
//     the per-atom decrypt in front — the KEY-PROVING check: it proves the key still
//     opens this archive, at one atom's egress.
//   - an encrypted stream-shape archive has no cheap structural sample: Ran=false.
//
// rot rotates the sampled unit across drills, like the checksum sample's part
// rotation. Ingredient gaps report Ran=false; a decode/list fault is the error; a
// clean decode whose members differ is Ran && !OK — the caller's integrity verdict.
func (r *Restorer) Sample(medium string, a record.Archive, opts crypt.Options, arch archiver.Archiver, rot int) (FrameSampleResult, error) {
	res := FrameSampleResult{Unit: "frame"}
	ref := archiveio.Ref{Run: a.Run, DLE: a.DLE, Level: a.Level}
	idx, err := r.deps.Store.Index(ref)
	if err != nil || len(idx.Members) == 0 {
		return res, nil
	}
	var table []record.Frame
	var decode func(rangeGroup, io.ReadCloser) io.ReadCloser
	switch {
	case a.Shape == record.ShapeAtomic:
		res.Unit = "atom"
		seals, aerr := r.deps.Store.AtomSeals(ref)
		if aerr != nil {
			return res, nil // seal-less copy: nothing to cut against
		}
		if table = a.Shape.RestartTable(nil, seals); table == nil {
			return res, nil
		}
		decrypt, _, derr := buildDecode(a.Compress, r.deps.CompressOpts, a.Encrypt, opts)
		if derr != nil {
			return res, derr
		}
		encSizes := sealSizes(seals)
		decode = func(g rangeGroup, rc io.ReadCloser) io.ReadCloser {
			return atomicPlaintext(rc, groupAtomSizes(table, encSizes, g), decrypt)
		}
	case a.Encrypt != "" && a.Encrypt != "none":
		return res, nil // encrypted stream shape: whole-archive checks only
	default:
		if table = a.Shape.RestartTable(idx.Frames, nil); table == nil {
			return res, nil
		}
	}
	var decompress programs.Cmd
	if a.Compress != "" && a.Compress != "none" {
		cf, ferr := compress.Filter(a.Compress, r.deps.CompressOpts)
		if ferr != nil {
			return res, ferr
		}
		decompress = cf.Reverse
	}
	out, err := r.sampleTable(ref, medium, table, idx.Members, arch, rot, decode, decompress)
	out.Unit = res.Unit
	return out, err
}

// sampleTable is the shared sampling core behind SampleFrame (framed: table = the
// index's frame table, no extra decode) and SampleAtom (atomic: table = the seals'
// cumulative sizes, decode = the per-atom decrypt loop): pick a rotated,
// egress-budgeted table entry holding member headers, fetch the covering encoded
// range, decode it, list it with the archiver, and compare names+offsets against the
// index slice.
func (r *Restorer) sampleTable(ref archiveio.Ref, medium string, frames []record.Frame, members []record.Member, arch archiver.Archiver, rot int, decode func(rangeGroup, io.ReadCloser) io.ReadCloser, decompress programs.Cmd) (FrameSampleResult, error) {
	res := FrameSampleResult{}
	// Pick the rotated frame; scan forward (wrapping) past frames that hold no member
	// header (a large file's middle) or whose span would blow the tier's bounded-egress
	// promise: a frame's span runs from its first member header to its LAST member's
	// extent end, and a huge member starting late in a frame drags the span out to its
	// whole body. Frames over the budget are skipped; if every member-bearing frame is
	// over it, the smallest-span one still runs (coverage beats silence — and the
	// fetched size is logged either way).
	stride := int64(0)
	if len(frames) > 1 {
		stride = frames[1].Raw - frames[0].Raw
	}
	rawEnd := frames[len(frames)-1].Raw + stride // estimate for spans running to the stream's end
	budget := 4 * stride
	choice, spanStart, spanEnd := -1, int64(0), int64(-1)
	bestLen := int64(-1)
	for probe := 0; probe < len(frames); probe++ {
		k := (rot + probe) % len(frames)
		sel := membersInFrame(members, frames, k)
		if len(sel) == 0 {
			continue
		}
		start := sel[0].Off
		end := int64(-1)
		if i := memberPos(members, sel[len(sel)-1].Path); i+1 < len(members) && members[i+1].Off >= 0 {
			end = members[i+1].Off
		}
		spanLen := rawEnd - start
		if end >= 0 {
			spanLen = end - start
		}
		if bestLen < 0 || spanLen < bestLen {
			choice, spanStart, spanEnd, bestLen = k, start, end, spanLen
		}
		if budget <= 0 || spanLen <= budget {
			choice, spanStart, spanEnd = k, start, end
			break
		}
	}
	if choice < 0 {
		return res, nil
	}
	res.Frame = choice
	sel := membersInFrame(members, frames, choice)
	span := media.Range{Off: spanStart} // open-ended when the span runs to the stream's end
	if spanEnd >= 0 {
		span.Len = spanEnd - spanStart
	}
	groups := planGroups(frames, []media.Range{span})
	if len(groups) != 1 {
		return res, nil
	}
	trailer := arch.SpliceTrailer()
	if trailer == nil {
		return res, nil // the archiver's streams do not splice: nothing to sample
	}
	res.Bytes = groups[0].enc.Len
	if res.Bytes <= 0 {
		res.Bytes = -1 // a to-the-end fetch of unknown size
	}
	rc, err := r.deps.Store.OpenRange(ref, medium, groups[0].enc)
	if err != nil {
		return res, nil // no ranged copy on this medium — an ingredient gap, not a fault
	}
	stream := rc
	if decode != nil {
		stream = decode(groups[0], rc)
	}
	pr, pw := io.Pipe()
	go func() {
		if err := emitGroup(groups[0], stream, decompress, pw); err != nil {
			stream.Close()
			pw.CloseWithError(err)
			return
		}
		stream.Close()
		if _, err := pw.Write(trailer); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	listed, lerr := arch.List(pr)
	pr.Close()
	if lerr != nil {
		return res, lerr
	}
	res.Ran = true
	res.OK, res.Detail = frameSliceMatches(sel, listed, spanStart)
	return res, nil
}

// membersInFrame returns the index slice of members whose header starts inside frame k.
func membersInFrame(members []record.Member, frames []record.Frame, k int) []record.Member {
	start := frames[k].Raw
	end := int64(-1)
	if k+1 < len(frames) {
		end = frames[k+1].Raw
	}
	var sel []record.Member
	for _, m := range members {
		if m.Off < 0 {
			return nil // offset-less index: nothing to anchor on
		}
		if m.Off >= start && (end < 0 || m.Off < end) {
			sel = append(sel, m)
		}
	}
	return sel
}

// memberPos finds a member's position in the stream-ordered list.
func memberPos(members []record.Member, path string) int {
	for i, m := range members {
		if m.Path == path {
			return i
		}
	}
	return -1
}

// frameSliceMatches compares the listed members of a sampled span against the index
// slice: same members in order, offsets matching relative to the span's start.
func frameSliceMatches(want []record.Member, got []record.Member, spanStart int64) (bool, string) {
	if len(want) != len(got) {
		return false, fmt.Sprintf("frame lists %d member(s), index records %d", len(got), len(want))
	}
	for i := range want {
		if want[i].Path != got[i].Path {
			return false, fmt.Sprintf("frame member %d is %q, index records %q", i, got[i].Path, want[i].Path)
		}
		if got[i].Off >= 0 && got[i].Off != want[i].Off-spanStart {
			return false, fmt.Sprintf("frame member %q at offset %d, index places it at %d", got[i].Path, got[i].Off, want[i].Off-spanStart)
		}
	}
	return true, ""
}

// emitGroup decodes one fetched range and copies its extents into pw, discarding the
// bytes between them and draining the tail (bounded by a frame) so the decode child
// consumes its whole input and exits cleanly — frames are self-terminating, so the
// child is never killed mid-stream.
func emitGroup(g rangeGroup, rc io.ReadCloser, decompress programs.Cmd, pw io.Writer) error {
	var raw io.Reader = rc
	wait := func() error { return nil }
	if decompress.Name != "" {
		out, w, err := programs.Local().RunPipe(context.Background(), rc, decompress)
		if err != nil {
			return err
		}
		defer out.Close()
		raw, wait = out, w
	}
	pos := g.rawStart
	for _, e := range g.extents {
		if skip := e.Off - pos; skip > 0 {
			if _, err := io.CopyN(io.Discard, raw, skip); err != nil {
				return err
			}
		}
		pos = e.Off
		if e.End() < 0 {
			n, err := io.Copy(pw, raw)
			pos += n
			if err != nil {
				return err
			}
		} else {
			if _, err := io.CopyN(pw, raw, e.Len); err != nil {
				return err
			}
			pos = e.End()
		}
	}
	// Drain the group's tail so a decode child sees its output fully consumed.
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return err
	}
	return wait()
}
