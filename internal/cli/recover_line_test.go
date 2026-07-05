package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
)

func TestScanArgsQuoting(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`add f.txt`, []string{"add", "f.txt"}},
		{`add a b c`, []string{"add", "a", "b", "c"}},
		{`add "My Photo.jpg"`, []string{"add", "My Photo.jpg"}},
		{`add 'My Photo.jpg'`, []string{"add", "My Photo.jpg"}},
		{`add My\ Photo.jpg`, []string{"add", "My Photo.jpg"}},
		{`add "a b" c\ d`, []string{"add", "a b", "c d"}},
		{`add "with \"quote\""`, []string{"add", `with "quote"`}},
		{`add ""`, []string{"add", ""}},
		{`settime 2026-07-04 20:36`, []string{"settime", "2026-07-04", "20:36"}},
		{`  leading   spaces  `, []string{"leading", "spaces"}},
	}
	for _, c := range cases {
		if got := scanArgs(c.line); !reflect.DeepEqual(got, c.want) {
			t.Errorf("scanArgs(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestScanTokensAtSep(t *testing.T) {
	// atSep distinguishes "add " (a fresh, empty token to complete) from "add" (still
	// completing the command word).
	if _, atSep := scanTokens("add"); atSep {
		t.Error(`"add" should not end on a separator`)
	}
	if _, atSep := scanTokens("add "); !atSep {
		t.Error(`"add " should end on a separator`)
	}
	// An unterminated quote keeps the open token so completion works mid-quote.
	toks, atSep := scanTokens(`add "My `)
	if atSep {
		t.Error(`open quote should not read as a separator`)
	}
	if len(toks) != 2 || toks[1].text != "My " {
		t.Errorf(`open-quote token = %+v, want text "My "`, toks)
	}
}

func TestQuoteArgRoundTrips(t *testing.T) {
	for _, s := range []string{"plain", "My Photo.jpg", "a'b", `a"b`, `a\b`, "with 'both\" kinds", ""} {
		q := quoteArg(s)
		got := scanArgs("cmd " + q)
		if len(got) != 2 || got[1] != s {
			t.Errorf("quoteArg(%q)=%q did not round-trip: scanArgs gave %q", s, q, got)
		}
	}
}

func TestCompletion(t *testing.T) {
	// A lone leaf match completes fully and adds a trailing space.
	if r, ok := completion("f", []string{"f.txt"}); !ok || r != "f.txt " {
		t.Errorf(`single leaf = %q,%v want "f.txt ",true`, r, ok)
	}
	// A lone directory match keeps the cursor at the "/" (no trailing space).
	if r, ok := completion("s", []string{"sub/"}); !ok || r != "sub/" {
		t.Errorf(`single dir = %q,%v want "sub/",true`, r, ok)
	}
	// A name with a space is quoted so it round-trips.
	if r, ok := completion("My", []string{"My Photo.jpg"}); !ok || r != `'My Photo.jpg' ` {
		t.Errorf(`spaced leaf = %q,%v want quoted`, r, ok)
	}
	// Several matches extend to the common prefix.
	if r, ok := completion("f", []string{"foo", "food"}); !ok || r != "foo" {
		t.Errorf(`common prefix = %q,%v want "foo",true`, r, ok)
	}
	// No shared progress past what's typed: no completion.
	if _, ok := completion("f", []string{"foo", "bar"}); ok {
		t.Error("divergent candidates should not complete")
	}
	if _, ok := completion("f", nil); ok {
		t.Error("no candidates should not complete")
	}
}

// spacedFixture builds an engine with one run whose source holds a file whose name
// contains a space — the case the shell must be able to select.
func spacedFixture(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "My Photo.jpg"), []byte("pixels"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}
	return eng, "localhost:" + src
}

// TestRecoverShellAddQuotedSpacedPath: a filename containing a space, added with quotes
// (or a backslash escape), must be selected — the shell tokenizer honors quoting rather
// than splitting the name on its space.
func TestRecoverShellAddQuotedSpacedPath(t *testing.T) {
	eng, dle := spacedFixture(t)
	out := runShellScript(t, eng, dle, "add \"My Photo.jpg\"\nadd My\\ Photo.jpg\nlist\nquit\n")
	if strings.Contains(out, "not found") {
		t.Fatalf("a quoted/escaped spaced path must be found:\n%s", out)
	}
	if !strings.Contains(out, "added /My Photo.jpg") {
		t.Fatalf("expected the spaced file to be added:\n%s", out)
	}
	if !strings.Contains(out, "  /My Photo.jpg") {
		t.Fatalf("expected the spaced file in the selection listing:\n%s", out)
	}
}
