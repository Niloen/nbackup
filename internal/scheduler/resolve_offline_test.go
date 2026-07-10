package scheduler

import (
	"testing"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

func TestResolveFromCatalogRebuildsRecordedSet(t *testing.T) {
	set := []catalog.ResolvedDLE{
		{DLE: "localhost__data_alpha", Host: "localhost", Source: "/data/alpha", DumpType: "default", Origin: "localhost:/data"},
		{DLE: "localhost__data", Host: "localhost", Source: "/data", DumpType: "default", Origin: "localhost:/data", Rest: true},
	}
	s := New(Deps{ResolvedSet: func() []catalog.ResolvedDLE { return set }})
	dles, fails, err := s.resolveFromCatalog()
	if err != nil || len(fails) != 0 {
		t.Fatalf("recorded-set resolve should not fail: err=%v fails=%v", err, fails)
	}
	if len(dles) != 2 {
		t.Fatalf("want 2 reconstructed DLEs, got %d", len(dles))
	}
	// The child is a plain unit; the remainder reconstructs Base==Source so IsRest holds.
	if dles[0].IsRest() {
		t.Errorf("child %s should not be a remainder", dles[0].ID())
	}
	if !dles[1].IsRest() {
		t.Errorf("remainder %s should report IsRest (Base==Source)", dles[1].ID())
	}
	if dles[0].DumpTypeName() != "default" || dles[0].Host != "localhost" {
		t.Errorf("reconstructed DLE lost host/dumptype: %+v", dles[0])
	}
}

func TestResolveFromCatalogFallsBackToScalarSources(t *testing.T) {
	// No recorded set (fresh/rebuilt catalog): scalar sources resolve with no I/O,
	// pattern sources cannot be enumerated offline and become source failures.
	cfgDLEs := []config.DLE{
		{Host: "localhost", Path: "/home", DumpType: "default"},
		{Host: "localhost", Path: "/data", DumpType: "default", Partition: "*"},
	}
	s := New(Deps{
		ResolvedSet: func() []catalog.ResolvedDLE { return nil },
		DLEs:        func() []config.DLE { return cfgDLEs },
	})
	dles, fails, err := s.resolveFromCatalog()
	if err != nil {
		t.Fatalf("fallback should not error: %v", err)
	}
	if len(dles) != 1 || dles[0].Source != "/home" {
		t.Fatalf("want the one scalar source /home, got %+v", dles)
	}
	if len(fails) != 1 || fails[0].Source.Path != "/data" {
		t.Fatalf("want the pattern source /data reported as a failure, got %+v", fails)
	}
}
