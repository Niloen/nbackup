package restorer

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

// atomic.go is the FRAMED-ATOMIC read path: an atomic archive's parts are sealed
// atoms — each one complete encrypted message — so the decode loop runs ONE decrypt
// child per atom (gpg rejects concatenated messages by design) and concatenates the
// plaintexts, which are whole compressed frames and decode as a single stream. The
// atom boundaries come from the per-part seals: their sizes cut the raw stream, their
// cumulative RawSize is the member→atom map selective restore plans over.

// atomSizes returns an atomic archive's per-atom encrypted sizes from its seals — the
// decode loop's cut list — or an error naming the remedy when no copy records them.
func (r *Restorer) atomSizes(ref archiveio.Ref) ([]int64, error) {
	seals, err := r.deps.Store.AtomSeals(ref)
	if err != nil {
		return nil, err
	}
	if len(seals) == 0 {
		return nil, fmt.Errorf("atomic archive %s %s L%d records no per-part seals on any copy — its atoms cannot be cut for decode; run `nb rebuild`", ref.Run, ref.DLE, ref.Level)
	}
	return sealSizes(seals), nil
}

// sealSizes lists the seals' part sizes — an atomic archive's atom cut list.
func sealSizes(seals []record.PartSeal) []int64 {
	sizes := make([]int64, len(seals))
	for i, s := range seals {
		sizes[i] = s.Size
	}
	return sizes
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

// groupAtomSizes lists the encrypted sizes of the atoms a group's encoded range
// covers, in order — the cut list for its per-atom decrypt loop.
func groupAtomSizes(atoms []record.Frame, encSizes []int64, g rangeGroup) []int64 {
	var out []int64
	var covered int64
	for i, a := range atoms {
		if a.Enc < g.enc.Off {
			continue
		}
		if g.enc.Len > 0 && covered >= g.enc.Len {
			break
		}
		out = append(out, encSizes[i])
		covered += encSizes[i]
	}
	return out
}
