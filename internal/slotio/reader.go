package slotio

import (
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Reader reads an archive's part stream back from media. It holds no state and no volume:
// an archive's parts may live on several volumes, so the caller supplies a PartOpener that
// mounts and opens each part in turn. Reading is parts-only — concatenate, assert headers,
// optionally re-hash. Reversing the payload's transforms (decrypt, decompress) is the
// engine's job: it composes them as host-placed hostexec stages over Open's raw stream, so
// decrypt runs where the key lives and decompress on the target.
type Reader struct{}

// NewReader returns a Reader.
func NewReader() *Reader { return &Reader{} }

// Expect is the identity a caller believes an archive's parts hold, asserted against
// each part file's actual header before its bytes are trusted. It is the cheap
// catch-all against a swapped volume or a stale catalog (the header is decoded
// anyway).
type Expect struct {
	Slot  string
	DLE   string
	Level int
}

// PartOpener mounts the volume a part lives on and opens its file, returning the
// file's header and a payload stream the caller closes. The engine implements it
// over the librarian (mount the part's volume, then ReadFile its position).
type PartOpener func(p record.FilePos) (record.Header, io.ReadCloser, error)

// Open returns an archive's payload as the ordered concatenation of its part files —
// still in on-medium form (compressed/encrypted), untransformed. It is the single read
// primitive: a copy re-splits these bytes onto a target without recompressing; a restore,
// deep verify, and the drill feed them into a host-placed decode pipeline. It primes the
// first part eagerly so a missing/wrong volume errors here, letting a copy-selecting caller
// fail over to another copy rather than discovering the fault only once bytes are pulled.
// Each part's header is asserted as it is reached. The caller closes the returned reader.
func (r *Reader) Open(parts []record.FilePos, want Expect, open PartOpener) (io.ReadCloser, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("archive %s %s L%d has no parts", want.Slot, want.DLE, want.Level)
	}
	raw := &partsReader{parts: parts, want: want, open: open}
	if err := raw.prime(); err != nil {
		return nil, err
	}
	return raw, nil
}

// VerifyParts asserts each part's header against want, then re-hashes the
// concatenated raw payloads and compares to sha.
func (r *Reader) VerifyParts(parts []record.FilePos, want Expect, sha string, open PartOpener) (bool, error) {
	raw := &partsReader{parts: parts, want: want, open: open}
	defer raw.Close()
	got, err := xfer.HashReader(raw)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// partsReader concatenates an archive's part payloads, opening each part lazily as
// the previous one is exhausted so that only one volume is mounted at a time. It
// asserts each part's header (identity + ascending part index) before its bytes flow.
type partsReader struct {
	parts []record.FilePos
	want  Expect
	open  PartOpener
	idx   int
	cur   io.ReadCloser
}

// prime opens the first part so an open-time error (missing/wrong volume) surfaces
// before any bytes are pulled, enabling the caller's copy-to-copy failover.
func (pr *partsReader) prime() error {
	if len(pr.parts) == 0 {
		return nil
	}
	h, rc, err := pr.open(pr.parts[0])
	if err != nil {
		return err
	}
	if err := assertPart(h, pr.want, 0); err != nil {
		rc.Close()
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
			h, rc, err := pr.open(pr.parts[pr.idx])
			if err != nil {
				return 0, err
			}
			if err := assertPart(h, pr.want, pr.idx); err != nil {
				rc.Close()
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
func assertPart(h record.Header, want Expect, part int) error {
	if h.Kind != record.KindArchive {
		return fmt.Errorf("position holds a %q record, not an archive", h.Kind)
	}
	if h.Slot != want.Slot || h.DLE != want.DLE || h.Level != want.Level {
		return fmt.Errorf("position holds %s %s L%d, expected %s %s L%d (wrong volume or stale catalog — run `nb rebuild`)",
			h.Slot, h.DLE, h.Level, want.Slot, want.DLE, want.Level)
	}
	if h.Part != part {
		return fmt.Errorf("position holds %s %s L%d part %d, expected part %d (wrong volume or stale catalog — run `nb rebuild`)",
			h.Slot, h.DLE, h.Level, h.Part, part)
	}
	return nil
}
