package slotio

import (
	"errors"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/crypt"
	"github.com/Niloen/nbackup/internal/filter"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/xfer"
)

// Reader reads slot contents back from media. It holds no volume — an archive's
// parts may live on several volumes, so the caller supplies a PartOpener that mounts
// and opens each part in turn.
type Reader struct {
	fopts filter.Options
	copts crypt.Options
}

// NewReader returns a Reader. fopts carries codec settings (e.g. a binary override)
// used when decompressing archives; copts carries the decryptor's key reference
// (e.g. a passphrase file) — public-key schemes need none.
func NewReader(fopts filter.Options, copts crypt.Options) *Reader {
	return &Reader{fopts: fopts, copts: copts}
}

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
type PartOpener func(p PartPosition) (media.Header, io.ReadCloser, error)

// OpenArchiveParts opens the plaintext stream of an archive whose payload is the
// ordered concatenation of parts. It reads each part fully before opening the next
// (a single drive holds only one volume at a time), asserting every part's header
// against want and its position in the sequence, then reverses the transforms over
// the whole concatenation in write order's inverse: decrypt, then decompress. The
// caller closes the returned reader.
func (r *Reader) OpenArchiveParts(parts []PartPosition, codec, encrypt string, want Expect, open PartOpener) (io.ReadCloser, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("archive %s %s L%d has no parts", want.Slot, want.DLE, want.Level)
	}
	raw := &partsReader{parts: parts, want: want, open: open}
	// Open the first part eagerly so a missing/wrong volume errors here, letting the
	// caller fail over to another copy — rather than surfacing only once a child
	// (or a stock reader) pulls bytes, by which point failover is impossible.
	if err := raw.prime(); err != nil {
		return nil, err
	}
	dec, err := crypt.Decrypt(encrypt, raw, r.copts)
	if err != nil {
		raw.Close()
		return nil, err
	}
	src, err := filter.Decompress(codec, dec, r.fopts)
	if err != nil {
		dec.Close()
		raw.Close()
		return nil, err
	}
	return multiCloser{Reader: src, closers: []io.Closer{src, dec, raw}}, nil
}

// OpenRawParts returns the archive's raw (still-compressed) payload as the ordered
// concatenation of its parts, without reversing the codec — the read side of a copy,
// which re-splits the same bytes onto the target without recompressing. Each part's
// header is asserted as it is reached. The caller closes the returned reader.
func (r *Reader) OpenRawParts(parts []PartPosition, want Expect, open PartOpener) io.ReadCloser {
	return &partsReader{parts: parts, want: want, open: open}
}

// VerifyParts asserts each part's header against want, then re-hashes the
// concatenated raw payloads and compares to sha.
func (r *Reader) VerifyParts(parts []PartPosition, want Expect, sha string, open PartOpener) (bool, error) {
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
	parts []PartPosition
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
func assertPart(h media.Header, want Expect, part int) error {
	if h.Kind != media.KindArchive {
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

// multiCloser adapts a reader plus the closers backing it into one ReadCloser.
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m multiCloser) Close() error {
	// Join every stage's error, not just the first: when the decryptor fails (e.g.
	// a wrong gpg key) the decompressor downstream of it also fails on the resulting
	// truncated stream, and the decryptor's message is the real cause — returning
	// only the first closer's error would hide it behind the decompressor's symptom.
	var errs []error
	for _, c := range m.closers {
		if e := c.Close(); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}
