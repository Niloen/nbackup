package xfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func reader(b []byte) Source { return Reader(io.NopCloser(bytes.NewReader(b))) }

// TestHashSinkMatch: a plain reader → no filters → Hash matches (a mismatch is the
// Sink-role error TestHashSinkMismatch covers).
func TestHashSinkMatch(t *testing.T) {
	data := []byte(strings.Repeat("the quick brown fox\n", 500))
	if _, err := Transfer(reader(data), NewFilters(), Hash(sha(data))); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
}

// TestHashSinkMismatch: a wrong checksum is a Sink-role failure.
func TestHashSinkMismatch(t *testing.T) {
	_, err := Transfer(reader([]byte("abc")), NewFilters(), Hash(sha([]byte("xyz"))))
	var te *Error
	if !errors.As(err, &te) || te.Role != RoleSink {
		t.Fatalf("want Sink-role error, got %v", err)
	}
}

// TestFiltersRoundTrip: filters run on the local server — gzip -c | gzip -dc is identity.
func TestFiltersRoundTrip(t *testing.T) {
	if _, err := programs.Local().Command("gzip", "--version").Output(); err != nil {
		t.Skip("gzip unavailable")
	}
	data := []byte(strings.Repeat("payload-", 4096))
	f := NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-c"}}).
		Add(programs.Cmd{Name: "gzip", Args: []string{"-dc"}})
	if _, err := Transfer(reader(data), f, Hash(sha(data))); err != nil {
		t.Fatalf("round-trip Transfer: %v", err)
	}
}

// TestFiltersFaultRole: decompressing non-gzip input fails in the Filters zone.
func TestFiltersFaultRole(t *testing.T) {
	if _, err := programs.Local().Command("gzip", "--version").Output(); err != nil {
		t.Skip("gzip unavailable")
	}
	f := NewFilters(programs.Cmd{Name: "gzip", Args: []string{"-dc"}})
	_, err := Transfer(reader([]byte("not gzip data")), f, Drain())
	var te *Error
	if !errors.As(err, &te) || te.Role != RoleFilters {
		t.Fatalf("want Filters-role error, got %v", err)
	}
}

// TestProgramSink: a program sink (cat) consumes the stream as stdin.
func TestProgramSink(t *testing.T) {
	data := []byte("hello program sink")
	sink := NewPrograms(programs.Local()).Add(programs.Cmd{Name: "cat"})
	if _, err := Transfer(reader(data), NewFilters(), sink); err != nil {
		t.Fatalf("program sink Transfer: %v", err)
	}
}
