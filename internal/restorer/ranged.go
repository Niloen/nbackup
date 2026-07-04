package restorer

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/transform/compress"
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

// rawExtent is one selected member's byte range in the raw archive stream; end < 0
// means "to the stream's end" (the last member's extent).
type rawExtent struct {
	start, end int64
}

// rangeGroup is one coalesced fetch: an encoded range to read off the medium (encLen
// < 0 = to the end), the raw offset its decoded output begins at, and the extents to
// emit from within it, ascending.
type rangeGroup struct {
	encOff, encLen int64
	rawStart       int64
	extents        []rawExtent
}

// planExtents resolves the selected member paths to their raw extents via the index's
// stream-ordered member list, coalescing adjacent extents. ok is false when any
// selected member is absent from the index or lacks an offset — the fallback cue.
func planExtents(members []record.Member, selected []string) ([]rawExtent, bool) {
	pos := make(map[string]int, len(members))
	for i, m := range members {
		pos[m.Path] = i
	}
	extents := make([]rawExtent, 0, len(selected))
	for _, path := range selected {
		i, found := pos[path]
		if !found || members[i].Off < 0 {
			return nil, false
		}
		end := int64(-1)
		if i+1 < len(members) {
			if members[i+1].Off < 0 {
				return nil, false
			}
			end = members[i+1].Off
		}
		extents = append(extents, rawExtent{start: members[i].Off, end: end})
	}
	sort.Slice(extents, func(i, j int) bool { return extents[i].start < extents[j].start })
	// Coalesce adjacent/overlapping extents (consecutive members selected together).
	out := extents[:0]
	for _, e := range extents {
		if n := len(out); n > 0 && (out[n-1].end < 0 || out[n-1].end >= e.start) {
			if out[n-1].end >= 0 && (e.end < 0 || e.end > out[n-1].end) {
				out[n-1].end = e.end
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
func planGroups(frames []record.Frame, extents []rawExtent) []rangeGroup {
	if frames == nil {
		out := make([]rangeGroup, 0, len(extents))
		for _, e := range extents {
			length := int64(-1)
			if e.end >= 0 {
				length = e.end - e.start
			}
			out = append(out, rangeGroup{encOff: e.start, encLen: length, rawStart: e.start, extents: []rawExtent{e}})
		}
		return out
	}
	var out []rangeGroup
	for _, e := range extents {
		// f: the last frame starting at or before the extent; g: the first frame at or
		// past its end (len(frames) = the stream's end when the extent runs into the tail).
		f := sort.Search(len(frames), func(i int) bool { return frames[i].Raw > e.start }) - 1
		if f < 0 {
			f = 0 // frame 0 is {0,0}; a start before it cannot happen, but stay in range
		}
		g := len(frames)
		if e.end >= 0 {
			g = sort.Search(len(frames), func(i int) bool { return frames[i].Raw >= e.end })
		}
		encEnd := int64(-1)
		if g < len(frames) {
			encEnd = frames[g].Enc
		}
		if n := len(out); n > 0 && (out[n-1].encLen < 0 || out[n-1].encOff+out[n-1].encLen >= frames[f].Enc) {
			// Overlaps/adjoins the previous group's frame window: merge the fetches.
			prev := &out[n-1]
			if prev.encLen >= 0 {
				if encEnd < 0 {
					prev.encLen = -1
				} else if encEnd > prev.encOff+prev.encLen {
					prev.encLen = encEnd - prev.encOff
				}
			}
			prev.extents = append(prev.extents, e)
			continue
		}
		length := int64(-1)
		if encEnd >= 0 {
			length = encEnd - frames[f].Enc
		}
		out = append(out, rangeGroup{encOff: frames[f].Enc, encLen: length, rawStart: frames[f].Raw, extents: []rawExtent{e}})
	}
	return out
}

// extractRanged attempts the ranged extraction of one step's selection. handled is
// false — with no error — when an ingredient is missing and the caller should fall
// back to the whole-stream path; once the first range is open (the capability probe),
// a failure is a real extraction error.
func (r *Restorer) extractRanged(st recovery.ExtractStep, d dest, log Logf) (handled bool, err error) {
	identity := (st.Compress == "" || st.Compress == "none") && (st.Encrypt == "" || st.Encrypt == "none")
	ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
	idx, ierr := r.deps.Store.Index(ref)
	if ierr != nil || len(idx.Members) == 0 {
		return false, nil
	}
	if len(idx.Frames) == 0 && !identity {
		return false, nil // no restart points and a real transform: whole stream only
	}
	extents, ok := planExtents(idx.Members, st.Members)
	if !ok {
		return false, nil
	}
	groups := planGroups(idx.Frames, extents)
	if len(groups) == 0 {
		return false, nil
	}
	// Capability probe: open the first group's range now. Any failure here — no seals,
	// a range-incapable medium, no copy — is the fallback cue, before any byte moved.
	first, perr := r.deps.Store.OpenRange(ref, "", groups[0].encOff, groups[0].encLen)
	if perr != nil {
		return false, nil
	}

	var decompress programs.Cmd
	if !identity {
		cf, err := compress.Filter(st.Compress, r.deps.CompressOpts)
		if err != nil {
			first.Close()
			return true, err
		}
		decompress = cf.Reverse
	}

	pr, pw := io.Pipe()
	go r.emitGroups(ref, groups, first, decompress, pw)

	arch, err := r.deps.ArchiverFor(st.Archiver, d.host)
	if err != nil {
		first.Close()
		pr.Close()
		return true, err
	}
	sink := xfer.NewProgramSink(d.exec).Add(arch.RestoreStage(d.dir, st.Members))
	_, terr := xfer.Transfer(context.Background(), xfer.Reader(pr), xfer.NewFilters(), sink)
	return true, terr
}

// emitGroups streams the planned groups into pw: per group it fetches the encoded
// range (the first is already open — the probe), decodes it with a fresh child when
// the pipeline has one, discards up to each extent, emits the extent, and drains the
// group's tail so the decode child exits cleanly at its input's end. A trailing 1 KiB
// of NULs is tar's end-of-archive marker (PoC-proven clean exit).
func (r *Restorer) emitGroups(ref archiveio.Ref, groups []rangeGroup, first io.ReadCloser, decompress programs.Cmd, pw *io.PipeWriter) {
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
		if err := emitGroup(g, rc, decompress, pw); err != nil {
			rc.Close()
			pw.CloseWithError(fmt.Errorf("ranged read of %s %s L%d: %w", ref.Run, ref.DLE, ref.Level, err))
			return
		}
		rc.Close()
	}
	// tar's end-of-archive: two 512-byte zero blocks (1 KiB) after the last member.
	if _, err := pw.Write(make([]byte, 1024)); err != nil {
		pw.CloseWithError(err)
		return
	}
	pw.Close()
}

// FrameSampleResult is one structural frame sample's outcome (see SampleFrame).
type FrameSampleResult struct {
	Ran    bool   // false: an ingredient was missing (no frames/offsets/ranged copy) — not a verdict
	OK     bool   // the listed members+offsets matched the index slice
	Detail string // the first mismatch, when !OK
	Frame  int    // the sampled frame's index
	Bytes  int64  // encoded bytes fetched (the sample's egress); -1 = a to-the-end fetch of unknown size
}

// SampleFrame structurally proves ONE frame group of an archive at bounded egress —
// the drill sample tier's framed sibling of the per-part seal check: it ranged-fetches
// the frames covering the members that start inside frame (rot % frames), decodes them
// through the real pipeline (a fresh child, exactly as a ranged restore would), lists
// the result with the archiver (`tar -tR` from the first indexed header), and compares
// the listed names AND offsets (relative to the span's start) against the index slice.
// rot rotates the sampled frame across drills, like the checksum sample's part
// rotation. Ingredient gaps report Ran=false; a decode/list fault is the error; a
// clean decode whose members differ is Ran && !OK — the caller's integrity verdict.
func (r *Restorer) SampleFrame(ref archiveio.Ref, medium, compressScheme string, arch archiver.Archiver, rot int) (FrameSampleResult, error) {
	res := FrameSampleResult{}
	idx, err := r.deps.Store.Index(ref)
	if err != nil || len(idx.Frames) == 0 || len(idx.Members) == 0 {
		return res, nil
	}
	var decompress programs.Cmd
	if compressScheme != "" && compressScheme != "none" {
		cf, ferr := compress.Filter(compressScheme, r.deps.CompressOpts)
		if ferr != nil {
			return res, ferr
		}
		decompress = cf.Reverse
	}
	return r.sampleTable(ref, medium, idx.Frames, idx.Members, arch, rot, nil, decompress)
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
	groups := planGroups(frames, []rawExtent{{start: spanStart, end: spanEnd}})
	if len(groups) != 1 {
		return res, nil
	}
	res.Bytes = groups[0].encLen
	rc, err := r.deps.Store.OpenRange(ref, medium, groups[0].encOff, groups[0].encLen)
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
		if _, err := pw.Write(make([]byte, 1024)); err != nil {
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
		if skip := e.start - pos; skip > 0 {
			if _, err := io.CopyN(io.Discard, raw, skip); err != nil {
				return err
			}
		}
		pos = e.start
		if e.end < 0 {
			n, err := io.Copy(pw, raw)
			pos += n
			if err != nil {
				return err
			}
		} else {
			if _, err := io.CopyN(pw, raw, e.end-e.start); err != nil {
				return err
			}
			pos = e.end
		}
	}
	// Drain the group's tail so a decode child sees its output fully consumed.
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return err
	}
	return wait()
}
