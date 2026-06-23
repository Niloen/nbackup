package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestSyncMirrorsLandingToTarget runs two backups onto disk, then exercises
// `nb sync`: a dry-run reports the backlog without copying, --apply mirrors both
// slots onto the archive medium (recording a second placement each), a re-sync is
// a no-op (idempotent), and --last bounds the selection to the most recent slot.
func TestSyncMirrorsLandingToTarget(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "sync me")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sync:    []config.SyncRule{{To: "archive"}},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	write(t, filepath.Join(src, "g.txt"), "more")
	s2, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}

	// Dry-run reports both slots and copies nothing.
	report, err := eng.SyncTo("", "archive", SyncSelection{}, false, false, nil)
	if err != nil {
		t.Fatalf("sync dry-run: %v", err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("dry-run backlog = %d, want 2", len(report.Items))
	}
	if report.Items[0].SlotID != s1.ID || report.Items[1].SlotID != s2.ID {
		t.Fatalf("backlog not oldest-first: %v", report.Items)
	}
	if eng.placedOn(s1.ID, "archive") {
		t.Fatal("dry-run must not copy")
	}

	// --apply mirrors both onto the archive medium.
	report, err = eng.SyncTo("", "archive", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync apply: %v", err)
	}
	if report.Copied() != 2 {
		t.Fatalf("copied = %d, want 2", report.Copied())
	}
	for _, id := range []string{s1.ID, s2.ID} {
		if !eng.placedOn(id, "archive") {
			t.Fatalf("slot %s not on archive after sync", id)
		}
		if got := len(eng.cat.Placements(id)); got != 2 {
			t.Fatalf("slot %s placements = %d, want 2", id, got)
		}
	}

	// A second sync is a no-op: nothing left to copy.
	report, err = eng.SyncTo("", "archive", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if len(report.Items) != 0 {
		t.Fatalf("re-sync backlog = %d, want 0 (idempotent)", len(report.Items))
	}
}

// TestSyncSelectionLast checks --last keeps only the most recent N slots.
func TestSyncSelectionLast(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "x")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	for d := 21; d <= 23; d++ {
		if _, err := eng.Run(time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC), nil); err != nil {
			t.Fatalf("dump %d: %v", d, err)
		}
	}

	report, err := eng.SyncTo("", "archive", SyncSelection{Last: 1}, false, false, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("--last 1 backlog = %d, want 1", len(report.Items))
	}
	if report.Items[0].SlotID != "slot-2026-06-23" {
		t.Fatalf("--last 1 kept %q, want the newest slot", report.Items[0].SlotID)
	}
}

// TestSyncFromNonLanding exercises an arbitrary --from: a slot is first mirrored
// disk -> archive, then re-mirrored archive -> cold with archive (not the landing
// medium) as the source. The cold copy must read from archive and land sealed.
func TestSyncFromNonLanding(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "tiered")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"cold":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Stage onto archive (from the landing medium), then onto cold *from archive*.
	if _, err := eng.SyncTo("", "archive", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync disk->archive: %v", err)
	}
	report, err := eng.SyncTo("archive", "cold", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync archive->cold: %v", err)
	}
	if report.From != "archive" || report.To != "cold" || report.Copied() != 1 {
		t.Fatalf("unexpected report: from=%q to=%q copied=%d", report.From, report.To, report.Copied())
	}
	if !eng.placedOn(s.ID, "cold") {
		t.Fatal("slot not on cold after archive->cold sync")
	}

	// The cold copy must restore on its own (proves the bytes came across intact).
	if _, err := eng.cat.RemovePlacement(s.ID, "disk"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.cat.RemovePlacement(s.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := eng.Restore(s.ID, config.DLE{Host: "h", Path: src}.Name(), dest, nil); err != nil {
		t.Fatalf("restore from cold copy: %v", err)
	}
	assertContent(t, filepath.Join(dest, "f.txt"), "tiered")
}

// TestSyncSpansLibraryVolumes syncs several disk slots onto a robotic tape library
// whose tapes are too small to hold them all: the changer must roll itself onto a
// fresh bay (auto-labeled) each time a tape fills, so every slot lands across
// distinct volumes. Each slot must then restore from the library on its own — proof
// the rolled-onto bytes are whole and the catalog points at the right tape.
func TestSyncSpansLibraryVolumes(t *testing.T) {
	src := t.TempDir()

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true, // let the changer label each fresh tape it rolls onto
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// Small tapes (256 KiB) across 4 bays: one slot fits, two do not.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4", "volume_size": "262144"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Three slots, each ~90 KiB of payload (codec none), so one fits on a 256 KiB
	// tape but two do not — the second slot forces a roll onto a fresh bay.
	payloads := map[string]string{}
	var ids []string
	for i, day := range []int{21, 22, 23} {
		body := strings.Repeat(string(rune('a'+i)), 90*1024)
		write(t, filepath.Join(src, "f.txt"), body)
		s, err := eng.Run(time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC), nil)
		if err != nil {
			t.Fatalf("dump %d: %v", day, err)
		}
		ids = append(ids, s.ID)
		payloads[s.ID] = body
	}

	// Seed the first (blank) bay so the library has a tape in the drive to start on;
	// the changer auto-labels and rolls onto the rest as each fills.
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	report, err := eng.SyncTo("", "lib", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync disk->lib: %v", err)
	}
	if report.Copied() != 3 {
		t.Fatalf("copied = %d, want 3", report.Copied())
	}

	// Every slot is on the library, and they are spread across more than one volume
	// (the whole point — a single tape could not hold them).
	vols := map[string]bool{}
	for _, id := range ids {
		placed := false
		for _, p := range eng.cat.Placements(id) {
			if p.Medium == "lib" {
				placed = true
				for _, v := range p.Volumes() {
					vols[v] = true
				}
			}
		}
		if !placed {
			t.Fatalf("slot %s not on the library after sync", id)
		}
	}
	if len(vols) < 2 {
		t.Fatalf("all slots landed on a single volume %v; the changer did not roll", vols)
	}

	// Drop the disk copies so restore must read from the library, exercising the
	// changer auto-mounting the right bay for each slot.
	name := config.DLE{Host: "h", Path: src}.Name()
	for _, id := range ids {
		if err := eng.vol.RemoveSlot(id); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		dest := t.TempDir()
		if err := eng.Restore(id, name, dest, nil); err != nil {
			t.Fatalf("restore %s from library: %v", id, err)
		}
		assertContent(t, filepath.Join(dest, "f.txt"), payloads[id])
	}
}

// TestSyncSlotOutOfTapes checks that a slot too big to fit even by spanning every
// available tape fails with an actionable "no further writable bay" error and is not
// recorded — the orphaned, unsealed parts left behind are reclaimable by relabel.
func TestSyncSlotOutOfTapes(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("z", 200*1024)) // ~200 KiB

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// One 64 KiB bay: far too small for the slot above, with no second bay to
			// span onto.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "1", "volume_size": "65536"}},
		},
		Sources: []config.DLE{{Host: "h", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.methodForDumpType(config.DefaultDumpType); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	_, err = eng.SyncTo("", "lib", SyncSelection{}, true, false, nil)
	if err == nil {
		t.Fatal("expected sync of an oversized slot to fail")
	}
	if !strings.Contains(err.Error(), "no further writable bay") {
		t.Fatalf("unexpected error: %v", err)
	}
	if eng.placedOn(s.ID, "lib") {
		t.Fatal("oversized slot must not be recorded on the library")
	}
}

// TestSyncTargetIsLanding rejects syncing a medium to itself.
func TestSyncTargetIsLanding(t *testing.T) {
	cfg := &config.Config{
		Landing: "disk",
		Media:   map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources: []config.DLE{{Host: "h", Path: t.TempDir()}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SyncTo("", "disk", SyncSelection{}, false, false, nil); err == nil {
		t.Fatal("expected error syncing the landing medium to itself")
	}
}
