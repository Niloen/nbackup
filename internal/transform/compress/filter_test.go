package compress

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// runFilter runs one filter command over src through the live programs path — the same
// RunPipe the engine drives — returning its output. An empty command (the none
// identity) passes src through unchanged.
func runFilter(t *testing.T, cmd programs.Cmd, src []byte) []byte {
	t.Helper()
	if cmd.Name == "" {
		return src
	}
	out, wait, err := programs.Local().RunPipe(bytes.NewReader(src), cmd)
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
// for every built-in scheme whose binary is available (none always is), driving the same
// Filter the engine uses.
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("the quick brown fox\n", 5000))
	for _, scheme := range []string{"none", "gzip", "zstd"} {
		scheme := scheme
		t.Run(scheme, func(t *testing.T) {
			if err := Check(scheme, Options{}); err != nil {
				t.Skipf("scheme unavailable: %v", err)
			}
			f, err := Filter(scheme, Options{Level: 3})
			if err != nil {
				t.Fatal(err)
			}
			compressed := runFilter(t, f.Forward, payload)
			got := runFilter(t, f.Reverse, compressed)
			if !bytes.Equal(got, payload) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			// A real compressor should shrink this very compressible payload.
			if scheme != "none" && len(compressed) >= len(payload) {
				t.Errorf("%s did not compress: %d >= %d", scheme, len(compressed), len(payload))
			}
		})
	}
}

// TestCheckUnknown rejects an unregistered scheme.
func TestCheckUnknown(t *testing.T) {
	if err := Check("brotli", Options{}); err == nil {
		t.Error("expected an error for an unknown scheme")
	}
}

// TestExt returns the archive extension per scheme.
func TestExt(t *testing.T) {
	cases := map[string]string{"zstd": "zst", "gzip": "gz", "none": ""}
	for scheme, want := range cases {
		got, err := Ext(scheme)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Ext(%q) = %q, want %q", scheme, got, want)
		}
	}
}
