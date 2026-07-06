package accounting

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// poolfake is a test-only medium type registered below: a 3-volume, 2 MB-per-reel
// pool, so accounting's PerVolume rollup can be exercised without depending on the real
// tape package (accounting must stay medium-neutral, keyed only on Volumes > 0).
func init() {
	media.Register(media.Spec{
		Type: "poolfake",
		Profile: func(media.Options) (media.Profile, error) {
			return media.NewVolumeProfile(3, 2_000_000), nil
		},
	})
}

func day(d int) time.Time { return time.Date(2026, 7, d, 12, 0, 0, 0, time.UTC) }

// TestSummarizeReflectsPrune is the point of the recorded ledger: growth is measured
// from the true curve, so a prune-driven decline yields no misleading fill projection
// where the retained-archive picture could never show a decline at all.
func TestSummarizeReflectsPrune(t *testing.T) {
	grown := Summarize([]catalog.UsageSample{
		{At: day(1), Medium: "disk", Used: 200},
		{At: day(11), Medium: "disk", Used: 1_200},
	}, 10_000)
	if grown.Samples != 2 {
		t.Fatalf("Samples = %d, want 2", grown.Samples)
	}
	if grown.PerDay != 100 { // (1200-200) over 10 days
		t.Errorf("PerDay = %d, want 100", grown.PerDay)
	}
	if grown.ProjFull.IsZero() {
		t.Errorf("expected a fill projection for a growing bounded medium")
	}

	// A net decline over the window (a big prune) yields no growth rate — the
	// projection must not run backwards.
	pruned := Summarize([]catalog.UsageSample{
		{At: day(1), Used: 5_000},
		{At: day(11), Used: 1_000},
	}, 10_000)
	if pruned.PerDay != 0 || !pruned.ProjFull.IsZero() {
		t.Errorf("declining series: PerDay=%d ProjFull=%v; want 0 / zero", pruned.PerDay, pruned.ProjFull)
	}
}

func TestSummarizeShortSpanNoRate(t *testing.T) {
	// Two samples hours apart is too short a baseline to read a daily rate from.
	st := Summarize([]catalog.UsageSample{
		{At: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Used: 100},
		{At: time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC), Used: 200},
	}, 1000)
	if st.PerDay != 0 || !st.ProjFull.IsZero() {
		t.Errorf("sub-day span: PerDay=%d ProjFull=%v; want 0 / zero", st.PerDay, st.ProjFull)
	}
	if st.Last != time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC) {
		t.Errorf("Last = %v; the span is still summarized", st.Last)
	}
}

// TestMediumStatsPerVolumePool: a labeled pool (three volumes: two full, one with
// room) reports the per-volume inventory keyed purely on the pool being non-empty,
// with a spanned archive's bytes attributed to only the first volume it lands on
// so the pool total is never double-counted.
func TestMediumStatsPerVolumePool(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vol-1", "vol-2", "vol-3"} {
		if err := cat.RecordVolume(record.Label{Name: name, Pool: "pool", Epoch: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// vol-1: one full-capacity archive.
	mustAdd(t, cat, record.Archive{Run: "run-2026-01-01.010000", DLE: "app", Level: 0, Compressed: 1_934_464, CreatedAt: day(1)},
		"pool", archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "vol-1", Epoch: 1, Pos: 1}}, Commit: archiveio.FilePos{Label: "vol-1", Epoch: 1, Pos: 2}})
	// vol-2: a spanned archive whose parts land on vol-2 and vol-3 — attributed
	// wholly to vol-2 (its first label), but counted as an archive/run on both.
	mustAdd(t, cat, record.Archive{Run: "run-2026-01-02.010000", DLE: "db", Level: 0, Compressed: 1_934_464, CreatedAt: day(2)},
		"pool", archiveio.ArchivePos{
			Parts:  []archiveio.FilePos{{Label: "vol-2", Epoch: 1, Pos: 1}, {Label: "vol-3", Epoch: 1, Pos: 1}},
			Commit: archiveio.FilePos{Label: "vol-3", Epoch: 1, Pos: 2},
		})

	cfg := &config.Config{Media: map[string]config.Media{"pool": {Type: "poolfake"}}}
	acct := New(Deps{Cat: cat, Cfg: cfg})

	st, ok := acct.MediumStats("pool")
	if !ok {
		t.Fatal("MediumStats(pool) not found")
	}
	if st.PoolVolumes != 3 {
		t.Fatalf("PoolVolumes = %d, want 3", st.PoolVolumes)
	}
	if len(st.PerVolume) != 3 {
		t.Fatalf("PerVolume has %d entries, want 3: %+v", len(st.PerVolume), st.PerVolume)
	}
	byLabel := map[string]VolumeUsage{}
	for _, v := range st.PerVolume {
		byLabel[v.Label] = v
	}
	const wantCap = 1_934_464 // volume_size 2_000_000 net of the 65536-byte framing overhead

	v1 := byLabel["vol-1"]
	if v1.Bytes != 1_934_464 || v1.Runs != 1 || v1.Archives != 1 || v1.Capacity != wantCap || v1.HasRoom {
		t.Errorf("vol-1 = %+v, want full (no room)", v1)
	}
	v2 := byLabel["vol-2"]
	if v2.Bytes != 1_934_464 || v2.Runs != 1 || v2.Archives != 1 || v2.HasRoom {
		t.Errorf("vol-2 = %+v, want the spanned archive's full bytes attributed here (no room)", v2)
	}
	v3 := byLabel["vol-3"]
	if v3.Bytes != 0 || v3.Runs != 1 || v3.Archives != 1 || !v3.HasRoom {
		t.Errorf("vol-3 = %+v, want 0 bytes (span attributed to vol-2) but the run/archive still counted, with room", v3)
	}

	// The pool's aggregate MediumInfo stays byte-for-byte too — the sum across
	// volumes is not double-counted just because a per-volume view is now available.
	if st.Used != 2*1_934_464 {
		t.Errorf("Used = %d, want %d (no double count of the spanned archive)", st.Used, 2*1_934_464)
	}
}

// TestMediumStatsPerVolumeAddressIdentifiedNil confirms disk/cloud media — which
// carry no volume labels — report a nil PerVolume, the regression guard that they
// keep rendering exactly as before this rollup existed.
func TestMediumStatsPerVolumeAddressIdentifiedNil(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, cat, record.Archive{Run: "run-2026-01-01.010000", DLE: "app", Level: 0, Compressed: 100, CreatedAt: day(1)},
		"disk", archiveio.ArchivePos{Commit: archiveio.FilePos{Pos: 1}})

	cfg := &config.Config{Media: map[string]config.Media{"disk": {Type: "disk"}}}
	acct := New(Deps{Cat: cat, Cfg: cfg})

	st, ok := acct.MediumStats("disk")
	if !ok {
		t.Fatal("MediumStats(disk) not found")
	}
	if st.PerVolume != nil {
		t.Errorf("PerVolume = %+v, want nil for an address-identified medium", st.PerVolume)
	}
	if st.PoolVolumes != 0 {
		t.Errorf("PoolVolumes = %d, want 0", st.PoolVolumes)
	}
}

func mustAdd(t *testing.T, cat *catalog.Catalog, a record.Archive, medium string, pos archiveio.ArchivePos) {
	t.Helper()
	if err := cat.AddArchive(a, medium, pos); err != nil {
		t.Fatalf("AddArchive: %v", err)
	}
}

func TestSummarizeUnboundedNoProjection(t *testing.T) {
	st := Summarize([]catalog.UsageSample{
		{At: day(1), Used: 100},
		{At: day(11), Used: 1_100},
	}, 0)
	if st.PerDay != 100 {
		t.Errorf("PerDay = %d, want 100 (growth is capacity-independent)", st.PerDay)
	}
	if !st.ProjFull.IsZero() {
		t.Errorf("unbounded medium projected full at %v; want none", st.ProjFull)
	}
}
