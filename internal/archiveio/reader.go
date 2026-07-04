package archiveio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/ratelimit"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// PartOpener mounts the volume a part lives on and opens its file, returning the
// file's header and a payload stream the caller closes. The fs implements it over an
// opened read medium (mount the part's volume, then read its position). It is the
// read side's device seam — addressed, not streaming: each call opens one named
// part, which is why it is an Opener and not a Source (the write-side dual, the
// PartAllocator, allocates instead: reads are random-access, writes append-ordered).
type PartOpener func(p FilePos) (record.Header, io.ReadCloser, error)

// Reader is one medium's read end — the mirror of Writer (one run's write end). It is
// bound to the medium at construction: open is the device seam each part is opened
// through, lim (nil = uncapped) paces the bytes read back, shared by every stream this
// Reader opens so concurrent reads from one medium share its budget. Each Open is one
// archive. Reading is parts-only — concatenate, assert headers, optionally re-hash.
// Reversing the payload's transforms (decrypt, decompress) is the operations' job: they
// compose them as host-placed program stages over Open's raw stream, so decrypt runs
// where the key lives and decompress on the target.
type Reader struct {
	open PartOpener
	lim  *ratelimit.Limiter
}

// NewReader binds a Reader to a medium's part opener and its bandwidth cap.
func NewReader(open PartOpener, lim *ratelimit.Limiter) *Reader {
	return &Reader{open: open, lim: lim}
}

// Open returns an archive's payload as the ordered concatenation of its part files —
// still in on-medium form (compressed/encrypted), untransformed. It is the single read
// primitive: a copy re-splits these bytes onto a target without recompressing; a restore,
// deep verify, and the drill feed them into a host-placed decode pipeline. It primes the
// first part eagerly so a missing/wrong volume errors here, letting a copy-selecting caller
// fail over to another copy rather than discovering the fault only once bytes are pulled.
// Each part's header is asserted as it is reached. The caller closes the returned reader.
func (r *Reader) Open(ref Ref, parts []FilePos) (io.ReadCloser, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("archive %s %s L%d has no parts", ref.Run, ref.DLE, ref.Level)
	}
	raw := &partsReader{parts: parts, want: ref, open: r.openLimited}
	if err := raw.prime(); err != nil {
		return nil, err
	}
	return raw, nil
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
// payloads and compares to sha.
func (r *Reader) Verify(ref Ref, parts []FilePos, sha string) (bool, error) {
	raw, err := r.Open(ref, parts)
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
// asserts each part's header (identity + ascending part index) before its bytes flow.
type partsReader struct {
	parts []FilePos
	want  Ref
	open  PartOpener
	idx   int
	cur   io.ReadCloser
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
	return rc, nil
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
		if err == io.EOF {
			closeErr := pr.cur.Close()
			pr.cur = nil
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
