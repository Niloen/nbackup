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
// detected inline as the part streams (Open with seals). It means corruption on this
// copy — an integrity fault, not a pipeline one — and callers classify it via
// errors.Is so a restore or verify reports "corrupt copy" rather than a downstream
// decode symptom.
var ErrSealMismatch = errors.New("part checksum mismatch vs its recorded seal")

// PartOpener mounts the volume a part lives on and opens its file, returning the
// file's header and a payload stream the caller closes. The fs implements it over an
// opened read medium (mount the part's volume, then read its position). It is the
// read side's device seam — addressed, not streaming: each call opens one named
// part, which is why it is an Opener and not a Source (the write-side dual, the
// PartAllocator, allocates instead: reads are random-access, writes append-ordered).
type PartOpener func(p FilePos) (record.Header, io.ReadCloser, error)

// RangedPartOpener is PartOpener for a byte sub-range of one part's payload
// (length < 0 = to the part's end) — the optional device capability behind ranged
// archive reads. nil when the medium cannot open ranges (tape); implementations
// surface media.ErrRangeUnsupported the same way, so callers fall back uniformly.
type RangedPartOpener func(p FilePos, off, length int64) (record.Header, io.ReadCloser, error)

// Reader is one medium's read end — the mirror of Writer (one run's write end). It is
// bound to the medium at construction: open is the device seam each part is opened
// through, lim (nil = uncapped) paces the bytes read back, shared by every stream this
// Reader opens so concurrent reads from one medium share its budget. Each Open is one
// archive. Reading is parts-only — concatenate, assert headers, optionally re-hash.
// Reversing the payload's transforms (decrypt, decompress) is the operations' job: they
// compose them as host-placed program stages over Open's raw stream, so decrypt runs
// where the key lives and decompress on the target.
type Reader struct {
	open      PartOpener
	openRange RangedPartOpener // nil = the medium cannot open payload sub-ranges
	lim       *ratelimit.Limiter
}

// NewReader binds a Reader to a medium's part opener, its optional ranged opener
// (nil when the medium cannot open payload sub-ranges), and its bandwidth cap.
func NewReader(open PartOpener, openRange RangedPartOpener, lim *ratelimit.Limiter) *Reader {
	return &Reader{open: open, openRange: openRange, lim: lim}
}

// Open returns an archive's payload as the ordered concatenation of its part files —
// still in on-medium form (compressed/encrypted), untransformed. It is the single read
// primitive: a copy re-splits these bytes onto a target without recompressing; a restore,
// deep verify, and the drill feed them into a host-placed decode pipeline. It primes the
// first part eagerly so a missing/wrong volume errors here, letting a copy-selecting caller
// fail over to another copy rather than discovering the fault only once bytes are pulled.
// Each part's header is asserted as it is reached.
//
// seals, when index-aligned with parts, arms inline integrity: each part is hashed as it
// streams and checked against its seal at the part's end, so a corrupt part surfaces as an
// ErrSealMismatch read error instead of silently feeding damaged bytes to the consumer
// (tar would happily restore a bit-flipped file body). nil (or misaligned — a sealless
// footer) reads unchecked, exactly as before. The caller closes the returned reader.
func (r *Reader) Open(ref Ref, parts []FilePos, seals []record.PartSeal) (io.ReadCloser, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("archive %s %s L%d has no parts", ref.Run, ref.DLE, ref.Level)
	}
	if len(seals) != len(parts) {
		seals = nil // seals must pair 1:1 with parts to mean anything
	}
	raw := &partsReader{parts: parts, want: ref, seals: seals, open: r.openLimited}
	if err := raw.prime(); err != nil {
		return nil, err
	}
	return raw, nil
}

// OpenRange returns a byte sub-range [off, off+length) of an archive's encoded payload
// (length < 0 = to the end) — the ranged-read primitive behind selective restore and
// frame sampling. The encoded offset is mapped to (part, in-part offset) through the
// cumulative part sizes the placement's seals record, so it requires seals aligned 1:1
// with the parts, plus a medium with the ranged part-open capability (else
// media.ErrRangeUnsupported surfaces from the opener and the caller falls back to
// Open). Each touched part's header is asserted as it is reached; the bytes stream
// UNSEALED — a sub-range cannot be checked against a whole-part seal — which is
// acceptable because ranged reads feed decode pipelines that fail loudly on damage,
// and integrity proper stays the drill/verify machinery's job.
func (r *Reader) OpenRange(ref Ref, parts []FilePos, seals []record.PartSeal, off, length int64) (io.ReadCloser, error) {
	if r.openRange == nil {
		return nil, media.ErrRangeUnsupported
	}
	if len(parts) == 0 || len(seals) != len(parts) {
		return nil, fmt.Errorf("archive %s %s L%d records no per-part seals; ranged reads need the parts' sizes", ref.Run, ref.DLE, ref.Level)
	}
	var total int64
	for _, s := range seals {
		total += s.Size
	}
	if off < 0 || off > total {
		return nil, fmt.Errorf("range offset %d outside archive payload (%d bytes)", off, total)
	}
	if length < 0 || off+length > total {
		length = total - off
	}
	var segs []rangeSegment
	start := int64(0)
	for i, s := range seals {
		end := start + s.Size
		if end > off && start < off+length && s.Size > 0 {
			segOff := max(off-start, 0)
			segEnd := min(off+length-start, s.Size)
			segs = append(segs, rangeSegment{idx: i, off: segOff, length: segEnd - segOff})
		}
		start = end
	}
	rr := &rangedPartsReader{parts: parts, want: ref, segs: segs, open: r.openRangeLimited}
	// Prime the first segment eagerly (as Open does for part 0): a capability or
	// mount fault must surface HERE, where the caller can still fail over to another
	// copy or fall back to the whole stream — not mid-read inside a decode pipeline.
	if err := rr.prime(); err != nil {
		return nil, err
	}
	return rr, nil
}

// openRangeLimited opens one part sub-range through the bound ranged opener and paces
// its stream to the medium's cap.
func (r *Reader) openRangeLimited(p FilePos, off, length int64) (record.Header, io.ReadCloser, error) {
	h, rc, err := r.openRange(p, off, length)
	if err != nil {
		return h, rc, err
	}
	return h, r.lim.ReadCloser(rc), nil
}

// rangeSegment is one part's slice of a ranged read: which part, and the byte window
// within its payload.
type rangeSegment struct {
	idx         int
	off, length int64
}

// rangedPartsReader concatenates the range's per-part segments, opening each lazily and
// asserting its header before bytes flow — the ranged sibling of partsReader.
type rangedPartsReader struct {
	parts []FilePos
	want  Ref
	segs  []rangeSegment
	open  RangedPartOpener
	pos   int
	cur   io.ReadCloser
}

// openSeg opens segment i and asserts its part's header before bytes flow.
func (rr *rangedPartsReader) openSeg(i int) (io.ReadCloser, error) {
	seg := rr.segs[i]
	h, rc, err := rr.open(rr.parts[seg.idx], seg.off, seg.length)
	if err != nil {
		return nil, err
	}
	if err := assertPart(h, rr.want, seg.idx); err != nil {
		rc.Close()
		return nil, err
	}
	return rc, nil
}

// prime opens the first segment so an open-time fault surfaces at OpenRange itself.
func (rr *rangedPartsReader) prime() error {
	if len(rr.segs) == 0 {
		return nil
	}
	rc, err := rr.openSeg(0)
	if err != nil {
		return err
	}
	rr.cur = rc
	return nil
}

func (rr *rangedPartsReader) Read(p []byte) (int, error) {
	for {
		if rr.cur == nil {
			if rr.pos >= len(rr.segs) {
				return 0, io.EOF
			}
			rc, err := rr.openSeg(rr.pos)
			if err != nil {
				return 0, err
			}
			rr.cur = rc
		}
		n, err := rr.cur.Read(p)
		if err == io.EOF {
			closeErr := rr.cur.Close()
			rr.cur = nil
			rr.pos++
			if n > 0 {
				return n, nil
			}
			if closeErr != nil {
				return 0, closeErr
			}
			continue
		}
		return n, err
	}
}

func (rr *rangedPartsReader) Close() error {
	if rr.cur != nil {
		err := rr.cur.Close()
		rr.cur = nil
		return err
	}
	return nil
}

// VerifyPart re-hashes ONE part's stored payload against its recorded seal — the
// bounded-egress integrity check: a sampling drill reads a single part off the medium
// instead of the whole archive. The part's header is asserted like any read; a size or
// checksum mismatch is a false verdict, not an error.
func (r *Reader) VerifyPart(ref Ref, parts []FilePos, idx int, seal record.PartSeal) (bool, error) {
	if idx < 0 || idx >= len(parts) {
		return false, fmt.Errorf("archive %s %s L%d has no part %d (%d part(s) recorded)", ref.Run, ref.DLE, ref.Level, idx, len(parts))
	}
	pr := &partsReader{parts: parts, want: ref, open: r.openLimited}
	rc, err := pr.openIdx(idx)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return false, err
	}
	return n == seal.Size && hex.EncodeToString(h.Sum(nil)) == seal.SHA256, nil
}

// Verify asserts each part's header against ref, then re-hashes the concatenated raw
// payloads and compares to sha. It reads unsealed — the whole-archive comparison below
// is its own verdict (a bool, not a read error), covering sealless archives too.
func (r *Reader) Verify(ref Ref, parts []FilePos, sha string) (bool, error) {
	raw, err := r.Open(ref, parts, nil)
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

// openLimited opens one part through the bound opener and paces its stream to the
// medium's cap.
func (r *Reader) openLimited(p FilePos) (record.Header, io.ReadCloser, error) {
	h, rc, err := r.open(p)
	if err != nil {
		return h, rc, err
	}
	return h, r.lim.ReadCloser(rc), nil
}

// partsReader concatenates an archive's part payloads, opening each part lazily as
// the previous one is exhausted so that only one volume is mounted at a time. It
// asserts each part's header (identity + ascending part index) before its bytes flow,
// and — when armed with seals — hashes each part inline and checks it at the part's
// end, so corruption is caught on the stream itself.
type partsReader struct {
	parts []FilePos
	want  Ref
	seals []record.PartSeal // nil = unchecked; else 1:1 with parts
	open  PartOpener
	idx   int
	cur   io.ReadCloser
	ph    hash.Hash // the in-flight part's running hash (seals only)
	pn    int64     // the in-flight part's byte count (seals only)
}

// openIdx opens part i and asserts its header (identity + part index) before its
// bytes flow — shared by prime and the read loop.
func (pr *partsReader) openIdx(i int) (io.ReadCloser, error) {
	h, rc, err := pr.open(pr.parts[i])
	if err != nil {
		return nil, err
	}
	if err := assertPart(h, pr.want, i); err != nil {
		rc.Close()
		return nil, err
	}
	if pr.seals != nil {
		pr.ph, pr.pn = sha256.New(), 0
	}
	return rc, nil
}

// checkSeal compares the finished part's inline hash against its recorded seal — the
// end-of-part integrity gate on the read stream.
func (pr *partsReader) checkSeal(i int) error {
	s := pr.seals[i]
	if pr.pn == s.Size && hex.EncodeToString(pr.ph.Sum(nil)) == s.SHA256 {
		return nil
	}
	return fmt.Errorf("%s %s L%d part %d of %d: %w — this copy is damaged; restore from another copy, then re-copy to repair this one",
		pr.want.Run, pr.want.DLE, pr.want.Level, i+1, len(pr.parts), ErrSealMismatch)
}

// prime opens the first part so an open-time error (missing/wrong volume) surfaces
// before any bytes are pulled, enabling the caller's copy-to-copy failover.
func (pr *partsReader) prime() error {
	if len(pr.parts) == 0 {
		return nil
	}
	rc, err := pr.openIdx(0)
	if err != nil {
		return err
	}
	pr.cur = rc
	return nil
}

func (pr *partsReader) Read(p []byte) (int, error) {
	for {
		if pr.cur == nil {
			if pr.idx >= len(pr.parts) {
				return 0, io.EOF
			}
			rc, err := pr.openIdx(pr.idx)
			if err != nil {
				return 0, err
			}
			pr.cur = rc
		}
		n, err := pr.cur.Read(p)
		if n > 0 && pr.seals != nil {
			pr.ph.Write(p[:n])
			pr.pn += int64(n)
		}
		if err == io.EOF {
			closeErr := pr.cur.Close()
			pr.cur = nil
			if pr.seals != nil {
				if serr := pr.checkSeal(pr.idx); serr != nil {
					return n, serr // fail the stream at the corrupt part, bytes already delivered or not
				}
			}
			pr.idx++
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
