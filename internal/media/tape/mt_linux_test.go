//go:build linux

package tape

import (
	"errors"
	"io"
	"testing"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
	"golang.org/x/sys/unix"
)

// TestRealDriveAcceptsVolumeSize: a real drive takes volume_size as its DECLARED
// per-cartridge capacity (Amanda's tapetype length) — the planner's reel size and
// the librarian's ledger-based fill arithmetic both hang off it. (openMT is lazy,
// so no hardware is needed to construct.)
func TestRealDriveAcceptsVolumeSize(t *testing.T) {
	v, err := newTapeVolume(media.Options{"device": "/dev/nst0", "volume_size": "4194304"}, "")
	if err != nil {
		t.Fatalf("a real drive should accept volume_size as declared capacity: %v", err)
	}
	st, ok := v.(*tapeChanger).drives[0].Loaded()
	if !ok {
		t.Fatal("a bare drive should always report its device")
	}
	if st.Capacity != 4194304 {
		t.Fatalf("declared capacity = %d, want 4194304", st.Capacity)
	}
}

// TestOpenMTBlockBounds covers the pure block-size validation in openMT: it defaults
// an unset size, floors it at the header block (a smaller record could split the
// fixed header across two reads), and caps it at the st driver's guaranteed
// single-buffer allocation.
func TestOpenMTBlockBounds(t *testing.T) {
	if _, err := openMT("/dev/nst0", 0); err != nil {
		t.Fatalf("block 0 should default, got %v", err)
	}
	// A defaulted device carries defaultTapeBlock.
	if d, _ := openMT("/dev/nst0", 0); d.block != defaultTapeBlock {
		t.Fatalf("default block = %d, want %d", d.block, defaultTapeBlock)
	}
	if _, err := openMT("/dev/nst0", record.HeaderBlock-1); err == nil {
		t.Fatal("a block below the header block should be rejected")
	}
	if _, err := openMT("/dev/nst0", maxTapeBlock+1); err == nil {
		t.Fatal("a block above the max should be rejected")
	}
	// Exactly at the header block and at the max are the inclusive boundaries.
	if _, err := openMT("/dev/nst0", record.HeaderBlock); err != nil {
		t.Fatalf("block == header block should be accepted, got %v", err)
	}
	if d, err := openMT("/dev/nst0", maxTapeBlock); err != nil || d.block != maxTapeBlock {
		t.Fatalf("block == max should be accepted, got block=%d err=%v", d.block, err)
	}
}

// TestIsSmallReadBuffer covers the errno classifier that triggers the read
// grow-and-retry: the three st(4) "record larger than the buffer" errnos are true,
// unrelated errors false.
func TestIsSmallReadBuffer(t *testing.T) {
	for _, e := range []error{unix.ENOMEM, unix.EOVERFLOW, unix.EINVAL} {
		if !isSmallReadBuffer(e) {
			t.Errorf("isSmallReadBuffer(%v) = false, want true", e)
		}
	}
	for _, e := range []error{unix.EIO, io.EOF, errors.New("boom"), nil} {
		if isSmallReadBuffer(e) {
			t.Errorf("isSmallReadBuffer(%v) = true, want false", e)
		}
	}
}

// TestGrowReadRecord covers the variable-block grow-and-retry loop without a drive:
// a record that fits is returned as-is; a too-small buffer grows (doubling, capped at
// maxTapeBlock) and re-reads the same record; a zero-length read is the trailing
// filemark (io.EOF); and a non-buffer error propagates unchanged.
func TestGrowReadRecord(t *testing.T) {
	t.Run("fits first try", func(t *testing.T) {
		buf := make([]byte, 8)
		gotBuf, n, err := growReadRecord(func(p []byte) (int, error) {
			return copy(p, []byte("abc")), nil
		}, buf)
		if err != nil || n != 3 {
			t.Fatalf("n=%d err=%v, want 3/nil", n, err)
		}
		if &gotBuf[0] != &buf[0] {
			t.Fatal("a fitting read should reuse the buffer, not reallocate")
		}
	})

	t.Run("grows and retries on ENOMEM", func(t *testing.T) {
		// First two attempts report the buffer too small; the third fits. The buffer
		// must double each retry (start 8 -> 16 -> 32) and the caller re-reads the same
		// record because a too-small read leaves the position unchanged.
		var sizes []int
		attempt := 0
		buf := make([]byte, 8)
		gotBuf, n, err := growReadRecord(func(p []byte) (int, error) {
			sizes = append(sizes, len(p))
			attempt++
			if attempt < 3 {
				return 0, unix.ENOMEM
			}
			return copy(p, make([]byte, 20)), nil
		}, buf)
		if err != nil {
			t.Fatalf("unexpected err %v", err)
		}
		if n != 20 {
			t.Fatalf("n=%d, want 20", n)
		}
		if len(gotBuf) != 32 {
			t.Fatalf("final buffer = %d, want 32 (8->16->32)", len(gotBuf))
		}
		want := []int{8, 16, 32}
		if len(sizes) != len(want) {
			t.Fatalf("read buffer sizes = %v, want %v", sizes, want)
		}
		for i := range want {
			if sizes[i] != want[i] {
				t.Fatalf("read %d buffer = %d, want %d", i, sizes[i], want[i])
			}
		}
	})

	t.Run("caps growth at maxTapeBlock", func(t *testing.T) {
		// A buffer already one doubling below the cap grows to exactly maxTapeBlock and,
		// still failing there, gives up (no growth past the driver's guaranteed alloc).
		attempt := 0
		buf := make([]byte, maxTapeBlock/2)
		gotBuf, _, err := growReadRecord(func(p []byte) (int, error) {
			attempt++
			return 0, unix.ENOMEM
		}, buf)
		if !errors.Is(err, unix.ENOMEM) {
			t.Fatalf("err = %v, want the ENOMEM once capped", err)
		}
		if len(gotBuf) != maxTapeBlock {
			t.Fatalf("buffer capped at %d, want %d", len(gotBuf), maxTapeBlock)
		}
		if attempt != 2 {
			t.Fatalf("attempts = %d, want 2 (once at half, once at cap)", attempt)
		}
	})

	t.Run("zero-length read is EOF", func(t *testing.T) {
		if _, n, err := growReadRecord(func(p []byte) (int, error) {
			return 0, nil
		}, make([]byte, 8)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("n=%d err=%v, want 0/EOF", n, err)
		}
		// An explicit io.EOF with no bytes is likewise the filemark.
		if _, n, err := growReadRecord(func(p []byte) (int, error) {
			return 0, io.EOF
		}, make([]byte, 8)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("n=%d err=%v, want 0/EOF", n, err)
		}
	})

	t.Run("non-buffer error propagates", func(t *testing.T) {
		if _, _, err := growReadRecord(func(p []byte) (int, error) {
			return 0, unix.EIO
		}, make([]byte, 8)); !errors.Is(err, unix.EIO) {
			t.Fatalf("err = %v, want EIO propagated (not retried)", err)
		}
	})
}
