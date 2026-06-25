package filter

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/streamproc"
)

// compress runs the codec's compressor over src using the read-side plumbing, the
// peer of the live CompressCmd path; it lets the round-trip test produce real
// compressed bytes without the deleted streaming Compress write-helper.
func compress(t *testing.T, codec string, o Options, src []byte) []byte {
	t.Helper()
	s, err := spec(codec)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := streamproc.ReadThrough(s.argv(s.compressArgv, o), o.Nice, bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRoundTrip checks compress -> decompress reproduces the input for every
// built-in codec whose binary is available (none always is).
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("the quick brown fox\n", 5000))
	for _, codec := range []string{"none", "gzip", "zstd"} {
		codec := codec
		t.Run(codec, func(t *testing.T) {
			if err := Check(codec, Options{}); err != nil {
				t.Skipf("codec unavailable: %v", err)
			}

			compressed := compress(t, codec, Options{Level: 3}, payload)

			rc, err := Decompress(codec, bytes.NewReader(compressed), Options{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if err := rc.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			// A real compressor should shrink this very compressible payload.
			if codec != "none" && len(compressed) >= len(payload) {
				t.Errorf("%s did not compress: %d >= %d", codec, len(compressed), len(payload))
			}
		})
	}
}

// TestCheckUnknown rejects an unregistered codec.
func TestCheckUnknown(t *testing.T) {
	if err := Check("brotli", Options{}); err == nil {
		t.Error("expected an error for an unknown codec")
	}
}

// TestExt returns the archive extension per codec.
func TestExt(t *testing.T) {
	cases := map[string]string{"zstd": "zst", "gzip": "gz", "none": ""}
	for codec, want := range cases {
		got, err := Ext(codec)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Ext(%q) = %q, want %q", codec, got, want)
		}
	}
}
