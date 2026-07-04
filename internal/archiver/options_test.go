package archiver

import (
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// TestOptionsBool: unset yields the default; the accepted spellings parse; a
// typo'd value is a loud error, not silently the default — matching how the
// registry's KnownOptions check rejects a typo'd key.
func TestOptionsBool(t *testing.T) {
	o := Options{"t": "yes", "f": "off", "bad": "ture"}
	if got, err := o.Bool("unset", true); err != nil || !got {
		t.Fatalf("unset: got %v, %v; want default true", got, err)
	}
	if got, err := o.Bool("t", false); err != nil || !got {
		t.Fatalf("yes: got %v, %v; want true", got, err)
	}
	if got, err := o.Bool("f", true); err != nil || got {
		t.Fatalf("off: got %v, %v; want false", got, err)
	}
	if _, err := o.Bool("bad", true); err == nil || !strings.Contains(err.Error(), `"ture"`) {
		t.Fatalf("a typo'd value must error naming it, got: %v", err)
	}
}

// TestCountFiles: directories (trailing slash, the member convention) are
// excluded, so one nested file counts as 1.
func TestCountFiles(t *testing.T) {
	members := []record.Member{{Path: "./etc/", Off: 0}, {Path: "./etc/hosts", Off: 512}, {Path: "./var/", Off: 1536}}
	if n := CountFiles(members); n != 1 {
		t.Fatalf("CountFiles = %d, want 1", n)
	}
}
