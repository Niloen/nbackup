package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// TestSyncMirrorsLandingToTarget runs two backups onto disk, then exercises
// `nb sync`: a dry-run reports the backlog without copying, a real run mirrors both
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
		Sync:     []config.SyncRule{{To: "archive"}},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
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

	// A real run mirrors both onto the archive medium.
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
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
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
	if report.Items[0].SlotID != "slot-2026-06-23.001" {
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
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
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
	if err := eng.Restore(s.ID, config.DLE{Host: "localhost", Path: src}.Name(), dest, false, nil); err != nil {
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
		// A one-day cycle makes every run a fresh full, so each slot carries its own
		// payload independently. (With incrementals the three same-second writes race
		// GNU tar's second-granularity change detection: a missed change leaves the
		// last incremental empty and the restore shows the previous day's contents.)
		Cycle: "1d",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// Small tapes (256 KiB) across 4 bays: one slot fits, two do not.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "4", "volume_size": "262144"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Three slots, each ~90 KiB of payload (scheme none), so one fits on a 256 KiB
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
				for _, v := range p.Labels() {
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
	name := config.DLE{Host: "localhost", Path: src}.Name()
	for _, id := range ids {
		removeSlotFiles(t, eng, id)
		if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		dest := t.TempDir()
		if err := eng.Restore(id, name, dest, false, nil); err != nil {
			t.Fatalf("restore %s from library: %v", id, err)
		}
		assertContent(t, filepath.Join(dest, "f.txt"), payloads[id])
	}
}

// TestRelabelRefusesProtectedSpanTape guards the relabel safety floor for spanned
// slots. A slot's seal record lives only on the last tape of its span, so the head
// and middle tapes hold payload parts but no seal. The relabel guard must still
// refuse them: it judges protection from the catalog (which records a placement on
// every tape the slot touches), not by scanning the mounted reel — scanning a
// sealless tape would find nothing and wrongly allow the wipe, destroying the
// spanned copy. --force remains the deliberate override.
func TestRelabelRefusesProtectedSpanTape(t *testing.T) {
	src := t.TempDir()
	// ~600 KiB across 256 KiB tapes: one slot spans three bays.
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("x", 600*1024))

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Cycle:     "1d",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "bays": "6", "volume_size": "262144"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	s, err := eng.Run(day, nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "bay-01", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}
	if _, err := eng.SyncTo("", "lib", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync disk->lib: %v", err)
	}

	// Volumes are recorded span order; index 0 is the head tape (a part, no seal).
	var spanVols []string
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lib" {
			spanVols = p.Labels()
		}
	}
	if len(spanVols) < 2 {
		t.Fatalf("slot did not span multiple tapes: %v", spanVols)
	}
	head := spanVols[0]

	// now is inside the run's minimum-age window, so the slot is protected. Under the
	// old reel-scanning guard the head tape looked empty and would be relabeled.
	if err := eng.LoadVolume("lib", head, true, nil); err != nil {
		t.Fatalf("load head tape %s: %v", head, err)
	}
	err = eng.LabelVolume("lib", head, true, false, day, nil)
	if err == nil {
		t.Fatalf("relabel of head span tape %s must be refused while the slot is protected", head)
	}
	if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("want a protected-slot refusal, got: %v", err)
	}

	// --force is the deliberate escape hatch and still works.
	if err := eng.LoadVolume("lib", head, true, nil); err != nil {
		t.Fatalf("reload head tape: %v", err)
	}
	if err := eng.LabelVolume("lib", head, true, true, day, nil); err != nil {
		t.Fatalf("--force relabel should succeed, got: %v", err)
	}

	// The relabel wiped a tape the span crossed, so the broken library copy must be
	// dropped from the catalog immediately — without a manual `nb rebuild` — even
	// though "lib" is not the landing medium. The disk copy is untouched.
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lib" {
			t.Fatalf("relabeling a span tape must drop the broken lib copy from the catalog, still have %+v", p)
		}
	}
	if got := eng.cat.Placements(s.ID); len(got) != 1 || got[0].Medium != "disk" {
		t.Fatalf("only the disk copy should remain, got %+v", got)
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
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
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
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SyncTo("", "disk", SyncSelection{}, false, false, nil); err == nil {
		t.Fatal("expected error syncing the landing medium to itself")
	}
}
