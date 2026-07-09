package scheduler

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
	errFor map[string]error            // per-key enumeration failures
}

func (f *fakeExpander) Expand(p archiver.SourcePattern) ([]archiver.Scope, error) {
	key := p.Pattern
	if p.Base != "" {
		key = p.Base + "|" + p.Pattern
	}
	if err := f.errFor[key]; err != nil {
		return nil, err
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

// TestResolvePlainKeepsIdentity: a plain source resolves 1:1, its slug equals the config
// declaration's (historical identity preserved), and it carries its Origin.
func TestResolvePlainKeepsIdentity(t *testing.T) {
	src := config.DLE{Host: "app01", Path: "/home", DumpType: "default"}
	out, fails, err := Resolve([]config.DLE{src}, expFor(&fakeExpander{}), noExcl)
	if err != nil || len(fails) != 0 {
		t.Fatalf("plain resolve: err=%v fails=%v", err, fails)
	}
	if len(out) != 1 || out[0].Name() != src.Name() || out[0].Origin != src.ID() {
		t.Fatalf("identity/origin wrong: %+v (want slug %s, origin %s)", out, src.Name(), src.ID())
	}
}

// TestResolveSourceFailureIsPerSource (the failure ladder's unit class): a source whose
// enumeration fails contributes NO units and is returned as a failure, while every other
// source resolves — one dead source never cancels the night.
func TestResolveSourceFailureIsPerSource(t *testing.T) {
	f := &fakeExpander{errFor: map[string]error{"/dataB|*": fmt.Errorf("host unreachable")}}
	srcs := []config.DLE{
		{Host: "fs", Path: "/dataA"},
		{Host: "fs", Path: "/dataB", Partition: "*"},
	}
	out, fails, err := Resolve(srcs, expFor(f), noExcl)
	if err != nil {
		t.Fatalf("a per-source failure must not fail resolution: %v", err)
	}
	if len(out) != 1 || out[0].Source != "/dataA" {
		t.Fatalf("the healthy source must resolve: %+v", out)
	}
	if len(fails) != 1 || fails[0].Source.ID() != "fs:/dataB" || !strings.Contains(fails[0].Err.Error(), "unreachable") {
		t.Fatalf("the dead source must be reported: %+v", fails)
	}
}

// TestResolveCollisionFails (config-class): two sources producing the same slug is a hard
// error naming both origins — never a silent merge of state chains, never a mere warning.
func TestResolveCollisionFails(t *testing.T) {
	f := &fakeExpander{scopes: map[string][]archiver.Scope{
		"/data|*": {
			{Base: "/data", Source: "/data/alice"},
			{Base: "/data", Source: "/data", Exclude: []string{"./alice"}},
		},
	}}
	srcs := []config.DLE{
		{Host: "fs", Path: "/data/alice", DumpType: "special"}, // explicit source
		{Host: "fs", Path: "/data", DumpType: "big", Partition: "*"},
	}
	_, _, err := Resolve(srcs, expFor(f), noExcl)
	if err == nil {
		t.Fatal("want a collision error, got nil")
	}
	for _, want := range []string{"fs-data-alice", "fs:/data/alice", "fs:/data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error should name %q; got: %v", want, err)
		}
	}
}
