package planner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/config"
)

// fakeExpander scripts Expand per source pattern, standing in for an archiver.
type fakeExpander struct {
	scopes map[string][]archiver.Scope // keyed by Pattern (or Base|Pattern for partitions)
	err    error
}

func (f *fakeExpander) Expand(p archiver.SourcePattern) ([]archiver.Scope, error) {
	if f.err != nil {
		return nil, f.err
	}
	key := p.Pattern
	if p.Base != "" {
		key = p.Base + "|" + p.Pattern
	}
	if sc, ok := f.scopes[key]; ok {
		return sc, nil
	}
	// identity default: a plain source resolves to itself
	return []archiver.Scope{{Source: p.Pattern, Exclude: p.Exclude}}, nil
}

func expFor(f *fakeExpander) ExpanderFor {
	return func(dumptype, host string) (Expander, error) { return f, nil }
}

func noExcl(string) []string { return nil }

// TestResolvePlainKeepsIdentity: a plain source resolves 1:1 and its slug equals the
// config declaration's — historical catalog/state identity is preserved.
func TestResolvePlainKeepsIdentity(t *testing.T) {
	src := config.DLE{Host: "app01", Path: "/home", DumpType: "default"}
	out, err := Resolve([]config.DLE{src}, expFor(&fakeExpander{}), noExcl)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 unit, got %d", len(out))
	}
	if out[0].Name() != src.Name() {
		t.Fatalf("slug drift: resolved %q != configured %q", out[0].Name(), src.Name())
	}
	if out[0].ID() != "app01:/home" || out[0].DumpTypeName() != "default" {
		t.Fatalf("identity wrong: %+v", out[0])
	}
}

// TestResolvePartitionEmitsUnits: a partition source becomes one unit per scope, the rest
// identified by IsRest, all inheriting host+dumptype.
func TestResolvePartitionEmitsUnits(t *testing.T) {
	f := &fakeExpander{scopes: map[string][]archiver.Scope{
		"/data|*": {
			{Base: "/data", Source: "/data/alice"},
			{Base: "/data", Source: "/data/bob"},
			{Base: "/data", Source: "/data", Exclude: []string{"/alice", "/bob"}},
		},
	}}
	out, err := Resolve([]config.DLE{{Host: "fs", Path: "/data", DumpType: "big", Partition: "*"}}, expFor(f), noExcl)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 units, got %d: %+v", len(out), out)
	}
	var rests int
	for _, d := range out {
		if d.Host != "fs" || d.DumpTypeName() != "big" {
			t.Errorf("unit %s lost host/dumptype: %+v", d.Name(), d)
		}
		if d.IsRest() {
			rests++
			if d.Name() != config.Slug("fs", "/data") {
				t.Errorf("the rest must carry the bare base slug, got %q", d.Name())
			}
		}
	}
	if rests != 1 {
		t.Errorf("want exactly one rest, got %d", rests)
	}
}

// TestResolveCollisionFails (R1): two sources producing the same slug is a hard error
// naming both origins — never a silent merge of state chains.
func TestResolveCollisionFails(t *testing.T) {
	f := &fakeExpander{scopes: map[string][]archiver.Scope{
		"/data|*": {
			{Base: "/data", Source: "/data/alice"},
			{Base: "/data", Source: "/data", Exclude: []string{"/alice"}},
		},
	}}
	srcs := []config.DLE{
		{Host: "fs", Path: "/data/alice", DumpType: "special"}, // explicit source
		{Host: "fs", Path: "/data", DumpType: "big", Partition: "*"},
	}
	_, err := Resolve(srcs, expFor(f), noExcl)
	if err == nil {
		t.Fatal("want a collision error, got nil")
	}
	for _, want := range []string{"fs-data-alice", "fs:/data/alice", "fs:/data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error should name %q; got: %v", want, err)
		}
	}
}

// TestResolveFailsLoud: an enumeration error fails the whole resolution — no fallback.
func TestResolveFailsLoud(t *testing.T) {
	f := &fakeExpander{err: fmt.Errorf("host unreachable")}
	_, err := Resolve([]config.DLE{{Host: "fs", Path: "/data", Partition: "*"}}, expFor(f), noExcl)
	if err == nil || !strings.Contains(err.Error(), "host unreachable") || !strings.Contains(err.Error(), "fs:/data") {
		t.Fatalf("want a loud, source-named error, got: %v", err)
	}
}
