package compress

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// runFilter runs one filter command over src through the live programs path — the same
// RunGrouped the engine drives — returning its output. An empty command (the none
// identity) passes src through unchanged.
func runFilter(t *testing.T, cmd programs.Cmd, src []byte) []byte {
	t.Helper()
	if cmd.Name == "" {
		return src
	}
	out, wait, err := programs.RunGrouped(bytes.NewReader(src), programs.Stage{Cmd: cmd, Exec: programs.Local()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRoundTrip checks Forward (compress) -> Reverse (decompress) reproduces the input
// for every built-in codec whose binary is available (none always is), driving the same
// Filter the engine uses.
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("the quick brown fox\n", 5000))
	for _, codec := range []string{"none", "gzip", "zstd"} {
		codec := codec
		t.Run(codec, func(t *testing.T) {
			if err := Check(codec, Options{}); err != nil {
				t.Skipf("codec unavailable: %v", err)
			}
			f, err := Filter(codec, Options{Level: 3})
			if err != nil {
				t.Fatal(err)
			}
			compressed := runFilter(t, f.Forward, payload)
			got := runFilter(t, f.Reverse, compressed)
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
