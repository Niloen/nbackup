package archiveio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// ErrSealMismatch marks a part whose stored bytes no longer match its recorded seal,
// detected inline as the part streams (a sealed part read whole). It means corruption
// on this copy — an integrity fault, not a pipeline one — and callers classify it via
// errors.Is so a restore or verify reports "corrupt copy" rather than a downstream
// decode symptom.
var ErrSealMismatch = errors.New("part checksum mismatch vs its recorded seal")

// Part is one part of an archive as it lies on a placement: the part file's position
// and its recorded seal. The seal rides WITH the part rather than in a parallel list,
// so a parts list can never misalign with its seals; the zero Seal means unsealed (a
// sealless footer's part), which simply reads unchecked.
type Part struct {
	Pos  FilePos
	Seal record.PartSeal
}

// Sealed reports whether the part carries a recorded seal.
func (p Part) Sealed() bool { return p.Seal.SHA256 != "" }

// BareParts wraps positional part locations as unsealed Parts — the write side's
// read-back vocabulary (an ArchivePos carries no seals; the copy's commit-time
// checksum covers that path).
func BareParts(pos []FilePos) []Part {
	parts := make([]Part, len(pos))
	for i, p := range pos {
		parts[i] = Part{Pos: p}
	}
	return parts
}

// Unsealed strips the parts' seals — a caller's explicit choice to read unchecked.
// Verify uses it: its whole-archive hash comparison is its own verdict (a bool), so a
// seal mismatch must not preempt it as a mid-stream read error.
func Unsealed(parts []Part) []Part {
	out := make([]Part, len(parts))
	for i, p := range parts {
		out[i] = Part{Pos: p.Pos}
	}
	return out
}

// PartOpener mounts the volume a part lives on and opens the rng slice of its file's
// payload (media.Range{} = the whole payload), returning the file's header and a
// stream the caller closes. The fs implements it over an opened read medium. It is the
// read side's device seam — addressed, not streaming: each call opens one named part,
// which is why it is an Opener and not a Source (the write-side dual, the
// PartAllocator, allocates instead: reads are random-access, writes append-ordered).
// A medium that cannot serve a genuine sub-range surfaces media.ErrRangeUnsupported,
// so callers fall back to the whole stream uniformly.
type PartOpener func(p FilePos, rng media.Range) (record.Header, io.ReadCloser, error)

// Reader is one medium's read end — the mirror of Writer (one run's write end). It is
// bound to the medium at construction: open is the device seam each part is opened
// through, lim (nil = uncapped) paces the bytes read back, shared by every stream this
// Reader opens so concurrent reads from one medium share its budget. Each Open is one
// archive. Reading is parts-only — concatenate, assert headers, seal-check what can
// be. Reversing the payload's transforms (decrypt, decompress) is the operations' job:
// they compose them as host-placed program stages over Open's raw stream, so decrypt
// runs where the key lives and decompress on the target.
type Reader struct {
	open PartOpener
	lim  *ratelimit.Limiter
}

// NewReader binds a Reader to a medium's part opener and its bandwidth cap.
func NewReader(open PartOpener, lim *ratelimit.Limiter) *Reader {
	return &Reader{open: open, lim: lim}
}

// Open returns the rng slice of an archive's payload — still in on-medium form
// (compressed/encrypted), untransformed — as the ordered concatenation of the covering
// segments of its part files. media.Range{} (the zero value) is the whole payload: the
// single read primitive a copy re-splits onto a target, a restore/deep-verify/drill
// decodes. A genuine sub-range is the ranged-read primitive behind selective restore
// and frame sampling: the offset maps to (part, in-part slice) through the part sizes
// the seals record — so a sub-range requires every part sealed — and needs a medium
// with the ranged capability (else media.ErrRangeUnsupported surfaces from the opener
// and the caller falls back to the whole stream).
//
// Integrity is per segment: a segment covering a WHOLE sealed part is hashed as it
// streams and checked against the seal at the part's end, so a corrupt part surfaces
// as an ErrSealMismatch read error instead of silently feeding damaged bytes onward
// (tar would happily restore a bit-flipped file body). A partial segment reads
// unchecked — a sub-range cannot be checked against a whole-part seal — which is
// acceptable because ranged reads feed decode pipelines that fail loudly on damage;
// integrity proper stays the drill/verify machinery's job. An unsealed part reads
// unchecked, exactly as before seals.
//
// The first segment is primed eagerly so an open-time fault (missing/wrong volume, a
// range-incapable medium) errors HERE, letting a copy-selecting caller fail over to
// another copy — or fall back to the whole stream — before any byte moved. Each
// touched part's header is asserted as it is reached. The caller closes the returned
// reader.
func (r *Reader) Open(ref Ref, parts []Part, rng media.Range) (io.ReadCloser, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("archive %s %s L%d has no parts", ref.Run, ref.DLE, ref.Level)
	}
	segs, err := planSegments(ref, parts, rng)
	if err != nil {
		return nil, err
	}
	pr := &partsReader{parts: parts, want: ref, segs: segs, open: r.openLimited}
	if err := pr.prime(); err != nil {
		return nil, err
	}
	return pr, nil
}

// segment is one part's slice of a read: which part, and the range within its payload
// (the zero Range = the whole part, which is what arms the part's seal check).
type segment struct {
	idx int
	rng media.Range
}

// planSegments resolves a read range to the covering per-part segments. A whole read
// is one whole-part segment per part and needs no sizes; a sub-range locates itself
// through the cumulative part sizes the seals record, so it requires every part
// sealed. A sub-range's segment that happens to cover its whole part is normalized to
// the whole-part form, so it regains the seal check for free.
func planSegments(ref Ref, parts []Part, rng media.Range) ([]segment, error) {
	if rng.IsWhole() {
		segs := make([]segment, len(parts))
		for i := range parts {
			segs[i] = segment{idx: i}
		}
		return segs, nil
	}
	var total int64
	for _, p := range parts {
		if !p.Sealed() {
			return nil, fmt.Errorf("archive %s %s L%d records no per-part seals; ranged reads need the parts' sizes", ref.Run, ref.DLE, ref.Level)
		}
		total += p.Seal.Size
	}
	b, err := rng.Bound(total)
	if err != nil {
		return nil, fmt.Errorf("archive %s %s L%d: %w", ref.Run, ref.DLE, ref.Level, err)
	}
	var segs []segment
	start := int64(0)
	for i, p := range parts {
		size := p.Seal.Size
		end := start + size
		if end > b.Off && start < b.Off+b.Len && size > 0 {
			segOff := max(b.Off-start, 0)
			segEnd := min(b.Off+b.Len-start, size)
			sr := media.Range{Off: segOff, Len: segEnd - segOff}
			if segOff == 0 && segEnd == size {
				sr = media.Range{} // the whole part: a plain read, seal check armed
			}
			segs = append(segs, segment{idx: i, rng: sr})
		}
		start = end
	}
	return segs, nil
}

// VerifyPart re-hashes ONE part's stored payload against its recorded seal — the
// bounded-egress integrity check: a sampling drill reads a single part off the medium
// instead of the whole archive. The part's header is asserted like any read; a size or
// checksum mismatch is a false verdict, not an error.
func (r *Reader) VerifyPart(ref Ref, parts []Part, idx int) (bool, error) {
	if idx < 0 || idx >= len(parts) {
		return false, fmt.Errorf("archive %s %s L%d has no part %d (%d part(s) recorded)", ref.Run, ref.DLE, ref.Level, idx, len(parts))
	}
	if !parts[idx].Sealed() {
		return false, fmt.Errorf("archive %s %s L%d records no seal for part %d", ref.Run, ref.DLE, ref.Level, idx)
	}
	// Read the part whole but unsealed: the comparison below is the verdict (a bool),
	// not a read error.
	pr := &partsReader{parts: Unsealed(parts), want: ref, segs: []segment{{idx: idx}}, open: r.openLimited}
	rc, err := pr.openSeg(0)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return false, err
	}
	seal := parts[idx].Seal
	return n == seal.Size && hex.EncodeToString(h.Sum(nil)) == seal.SHA256, nil
}

// Verify asserts each part's header against ref, then re-hashes the concatenated raw
// payloads and compares to sha. It reads unsealed — the whole-archive comparison below
// is its own verdict (a bool, not a read error), covering sealless archives too.
func (r *Reader) Verify(ref Ref, parts []Part, sha string) (bool, error) {
	raw, err := r.Open(ref, Unsealed(parts), media.Range{})
	if err != nil {
		return false, err
	}
	defer raw.Close()
	got, err := xfer.SHA256(raw)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// openLimited opens one part slice through the bound opener and paces its stream to
// the medium's cap.
func (r *Reader) openLimited(p FilePos, rng media.Range) (record.Header, io.ReadCloser, error) {
	h, rc, err := r.open(p, rng)
	if err != nil {
		return h, rc, err
	}
	return h, r.lim.ReadCloser(rc), nil
}

// partsReader concatenates a read's per-part segments, opening each lazily as the
// previous one is exhausted so that only one volume is mounted at a time. It asserts
// each touched part's header (identity + part index) before its bytes flow, and — for
// a whole-part segment of a sealed part — hashes the segment inline and checks it at
// the part's end, so corruption is caught on the stream itself.
type partsReader struct {
	parts []Part
	want  Ref
	segs  []segment
	open  PartOpener
	pos   int // next segment to open
	cur   io.ReadCloser
	ph    hash.Hash // the in-flight segment's running hash (sealed whole parts only)
	pn    int64     // the in-flight segment's byte count
}

// openSeg opens segment i, asserts its part's header before bytes flow, and arms the
// inline seal check when the segment is a sealed whole part.
func (pr *partsReader) openSeg(i int) (io.ReadCloser, error) {
	seg := pr.segs[i]
	h, rc, err := pr.open(pr.parts[seg.idx].Pos, seg.rng)
	if err != nil {
		return nil, err
	}
	if err := assertPart(h, pr.want, seg.idx); err != nil {
		rc.Close()
		return nil, err
	}
	pr.ph, pr.pn = nil, 0
	if seg.rng.IsWhole() && pr.parts[seg.idx].Sealed() {
		pr.ph = sha256.New()
	}
	return rc, nil
}

// checkSeal compares the finished whole-part segment's inline hash against its part's
// seal — the end-of-part integrity gate on the read stream.
func (pr *partsReader) checkSeal(i int) error {
	idx := pr.segs[i].idx
	s := pr.parts[idx].Seal
	if pr.pn == s.Size && hex.EncodeToString(pr.ph.Sum(nil)) == s.SHA256 {
		return nil
	}
	return fmt.Errorf("%s %s L%d part %d of %d: %w — this copy is damaged; restore from another copy, then re-copy to repair this one",
		pr.want.Run, pr.want.DLE, pr.want.Level, idx+1, len(pr.parts), ErrSealMismatch)
}

// prime opens the first segment so an open-time error (missing/wrong volume, a
// range-incapable medium) surfaces before any bytes are pulled, enabling the caller's
// copy-to-copy failover or whole-stream fallback.
func (pr *partsReader) prime() error {
	if len(pr.segs) == 0 {
		return nil
	}
	rc, err := pr.openSeg(0)
	if err != nil {
		return err
	}
	pr.cur = rc
	return nil
}

func (pr *partsReader) Read(p []byte) (int, error) {
	for {
		if pr.cur == nil {
			if pr.pos >= len(pr.segs) {
				return 0, io.EOF
			}
			rc, err := pr.openSeg(pr.pos)
			if err != nil {
				return 0, err
			}
			pr.cur = rc
		}
		n, err := pr.cur.Read(p)
		if n > 0 && pr.ph != nil {
			pr.ph.Write(p[:n])
			pr.pn += int64(n)
		}
		if err == io.EOF {
			closeErr := pr.cur.Close()
			pr.cur = nil
			if pr.ph != nil {
				if serr := pr.checkSeal(pr.pos); serr != nil {
					return n, serr // fail the stream at the corrupt part, bytes already delivered or not
				}
			}
			pr.pos++
			if n > 0 {
				return n, nil // surface EOF on the next call
			}
			if closeErr != nil {
				return 0, closeErr
			}
			continue
		}
		return n, err
	}
}

func (pr *partsReader) Close() error {
	if pr.cur != nil {
		err := pr.cur.Close()
		pr.cur = nil
		return err
	}
	return nil
}

// assertPart confirms a part file's header is the archive part the catalog expected:
// the right archive identity and the right index in the sequence. A mismatch means
// the wrong volume is mounted or the catalog is stale.
func assertPart(h record.Header, want Ref, part int) error {
	if h.Kind != record.KindArchive {
		return fmt.Errorf("position holds a %q record, not an archive", h.Kind)
	}
	if h.Run != want.Run || h.DLE != want.DLE || h.Level != want.Level {
		return fmt.Errorf("position holds %s %s L%d, expected %s %s L%d (wrong volume or stale catalog — run `nb rebuild`)",
			h.Run, h.DLE, h.Level, want.Run, want.DLE, want.Level)
	}
	if h.Part != part {
		return fmt.Errorf("position holds %s %s L%d part %d, expected part %d (wrong volume or stale catalog — run `nb rebuild`)",
			h.Run, h.DLE, h.Level, h.Part, part)
	}
	return nil
}
