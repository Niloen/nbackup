package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
)

// TestSyncMirrorsLandingToTarget runs two backups onto disk, then exercises
// `nb sync`: a dry-run reports the backlog without copying, a real run mirrors both
// runs onto the archive medium (recording a second placement each), a re-sync is
// a no-op (idempotent), and --last bounds the selection to the most recent run.
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	write(t, filepath.Join(src, "g.txt"), "more")
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}

	// Dry-run reports both runs and copies nothing.
	report, err := eng.SyncTo("", "archive", SyncSelection{}, false, false, nil)
	if err != nil {
		t.Fatalf("sync dry-run: %v", err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("dry-run backlog = %d, want 2", len(report.Items))
	}
	if report.Items[0].RunID != s1.ID || report.Items[1].RunID != s2.ID {
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
			t.Fatalf("run %s not on archive after sync", id)
		}
		if got := len(eng.cat.Placements(id)); got != 2 {
			t.Fatalf("run %s placements = %d, want 2", id, got)
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

// TestSyncSelectionLast checks --last keeps only the most recent N runs.
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	for d := 21; d <= 23; d++ {
		if _, err := eng.Run(context.Background(), time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC), nil); err != nil {
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
	if report.Items[0].RunID != "run-2026-06-23.001" {
		t.Fatalf("--last 1 kept %q, want the newest run", report.Items[0].RunID)
	}
}

// TestSyncSelectionSince exercises applySelection's Since filter: only runs whose
// logical date is at or after the bound are in the backlog (the older run is excluded),
// filtering on the run's backup date rather than its physical seal time.
func TestSyncSelectionSince(t *testing.T) {
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	for d := 21; d <= 23; d++ {
		if _, err := eng.Run(context.Background(), time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC), nil); err != nil {
			t.Fatalf("dump %d: %v", d, err)
		}
	}

	// Since 2026-06-22 keeps the 22nd and 23rd runs, excluding the 21st.
	report, err := eng.SyncTo("", "archive", SyncSelection{Since: time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)}, false, false, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(report.Items) != 2 {
		t.Fatalf("--since backlog = %d, want 2 (the 22nd and 23rd)", len(report.Items))
	}
	if report.Items[0].RunID != "run-2026-06-22.001" {
		t.Fatalf("oldest kept run = %q, want run-2026-06-22.001", report.Items[0].RunID)
	}
}

// TestSyncOverCapacityStillCopies exercises the capacity projection: syncing a backlog
// bigger than the target's capacity surfaces the overshoot (OverCapacity, ProjectedBytes)
// yet still copies — sync does not prune — recording CopiedBytes for the run record.
func TestSyncOverCapacityStillCopies(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("payload-", 4096)) // ~32 KiB, well past a 10-byte cap

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// A cloud target with a 10-byte capacity: any real run overshoots it.
			"tiny": {Type: "cloud", Capacity: "10", Params: map[string]string{"url": "file://" + t.TempDir()}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	report, err := eng.SyncTo("", "tiny", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync should copy despite overshoot: %v", err)
	}
	if !report.OverCapacity() {
		t.Errorf("backlog should overshoot the 10-byte target: projected=%d cap=%d", report.ProjectedBytes, report.TargetCapacity)
	}
	if report.ProjectedBytes <= report.TargetCapacity {
		t.Errorf("projected bytes %d should exceed capacity %d", report.ProjectedBytes, report.TargetCapacity)
	}
	if report.Copied() != 1 || report.CopiedBytes() <= 0 {
		t.Errorf("sync must still copy: copied=%d bytes=%d", report.Copied(), report.CopiedBytes())
	}
}

// TestSyncFromNonLanding exercises an arbitrary --from: a run is first mirrored
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
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
		t.Fatal("run not on cold after archive->cold sync")
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

// TestSyncSpansLibraryVolumes syncs several disk runs onto a robotic tape library
// whose tapes are too small to hold them all: the changer must roll itself onto a
// fresh bay (auto-labeled) each time a tape fills, so every run lands across
// distinct volumes. Each run must then restore from the library on its own — proof
// the rolled-onto bytes are whole and the catalog points at the right tape.
func TestSyncSpansLibraryVolumes(t *testing.T) {
	src := t.TempDir()

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true, // let the changer label each fresh tape it rolls onto
		// A one-day cycle makes every run a fresh full, so each run carries its own
		// payload independently. (With incrementals the three same-second writes race
		// GNU tar's second-granularity change detection: a missed change leaves the
		// last incremental empty and the restore shows the previous day's contents.)
		Cycle: "1d",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// Small tapes (256 KiB) across 4 bays: one run fits, two do not.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "slots": "4", "volume_size": "262144"}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Three runs, each ~90 KiB of payload (scheme none), so one fits on a 256 KiB
	// tape but two do not — the second run forces a roll onto a fresh bay.
	payloads := map[string]string{}
	var ids []string
	for i, day := range []int{21, 22, 23} {
		body := strings.Repeat(string(rune('a'+i)), 90*1024)
		write(t, filepath.Join(src, "f.txt"), body)
		s, err := eng.Run(context.Background(), time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC), nil)
		if err != nil {
			t.Fatalf("dump %d: %v", day, err)
		}
		ids = append(ids, s.ID)
		payloads[s.ID] = body
	}

	// Seed the first (blank) bay so the library has a tape in the drive to start on;
	// the changer auto-labels and rolls onto the rest as each fills.
	if err := eng.LoadVolume("lib", "1", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	report, err := eng.SyncTo("", "lib", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync disk->lib: %v", err)
	}
	if report.Copied() != 3 {
		t.Fatalf("copied = %d, want 3", report.Copied())
	}

	// Every run is on the library, and they are spread across more than one volume
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
			t.Fatalf("run %s not on the library after sync", id)
		}
	}
	if len(vols) < 2 {
		t.Fatalf("all runs landed on a single volume %v; the changer did not roll", vols)
	}

	// Drop the disk copies so restore must read from the library, exercising the
	// changer auto-mounting the right bay for each run.
	name := config.DLE{Host: "localhost", Path: src}.Name()
	for _, id := range ids {
		removeRunFiles(t, eng, id)
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

// TestMultiDriveCopyToTape copies one slot (several archives) from disk onto a multi-drive tape
// library and checks the copy spread the archives across the drives — the slot's target placement
// spans more than one tape — and that every DLE restores byte-for-byte from the library. It proves
// copy/sync go through the spool (drive per concurrent copy), not the old single-drive writer.
func TestMultiDriveCopyToTape(t *testing.T) {
	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// Two drives fed from four slots; tapes big enough that each drive's share fits
			// without spanning, so the spread we observe is across drives, not rolls.
			"lib": {Type: "tape", Params: map[string]string{
				"dir": t.TempDir(), "slots": "4", "drives": "2", "volume_size": "1048576"}},
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	names := []string{"alpha", "bravo", "charlie", "delta"}
	bodies := map[string]string{}
	src := map[string]string{}
	for i, n := range names {
		dir := t.TempDir()
		body := strings.Repeat(string(rune('a'+i)), 20*1024)
		write(t, filepath.Join(dir, n+".txt"), body)
		bodies[n] = body
		src[n] = dir
		cfg.Sources = append(cfg.Sources, config.DLE{Host: "localhost", Path: dir})
	}

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump to disk: %v", err)
	}

	// Copy the whole slot onto the library — the concurrent, multi-drive copy path.
	if err := eng.CopyRun(s.ID, "disk", "lib", false, nil); err != nil {
		t.Fatalf("copy disk->lib: %v", err)
	}

	// The copy landed on more than one tape (one per drive).
	vols := map[string]bool{}
	for _, p := range eng.cat.Placements(s.ID) {
		if p.Medium == "lib" {
			for _, v := range p.Labels() {
				vols[v] = true
			}
		}
	}
	if len(vols) < 2 {
		t.Fatalf("expected the copy to span >= 2 tapes (one per drive), got %d: %v", len(vols), vols)
	}

	// Drop the disk copies so restore must read the library — each DLE byte-for-byte, including
	// any whose tape sits in a non-zero drive.
	removeRunFiles(t, eng, s.ID)
	if _, err := eng.cat.RemovePlacement(s.ID, "disk"); err != nil {
		t.Fatal(err)
	}
	for i, d := range cfg.Sources {
		dest := t.TempDir()
		if err := eng.Restore(s.ID, d.Name(), dest, false, nil); err != nil {
			t.Fatalf("restore %s from library: %v", d.Name(), err)
		}
		assertContent(t, filepath.Join(dest, names[i]+".txt"), bodies[names[i]])
	}
	_ = src
}

// TestMultiDriveSyncCrossRun syncs several single-archive runs onto a multi-drive tape library in
// one spool run and checks the archives spread across the drives (more than one tape holds copies) —
// proving sync saturates the drives across slot boundaries, not one archive per slot serially — and
// that every run restores byte-for-byte from the library. It also guards the cross-run keying:
// each archive must record under its own run, not a shared sync id.
func TestMultiDriveSyncCrossRun(t *testing.T) {
	src := t.TempDir()
	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Cycle:     "1d", // each run a fresh full, so each slot carries its own payload independently
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib": {Type: "tape", Params: map[string]string{
				"dir": t.TempDir(), "slots": "6", "drives": "2", "volume_size": "1048576"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: src}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	cfg.Parallelism.Workers = 2

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	// Three separate runs → three single-archive runs on disk, each with distinct content.
	payloads := map[string]string{}
	var ids []string
	for i, day := range []int{21, 22, 23} {
		body := strings.Repeat(string(rune('a'+i)), 20*1024)
		write(t, filepath.Join(src, "f.txt"), body)
		s, err := eng.Run(context.Background(), time.Date(2026, 6, day, 0, 0, 0, 0, time.UTC), nil)
		if err != nil {
			t.Fatalf("dump %d: %v", day, err)
		}
		ids = append(ids, s.ID)
		payloads[s.ID] = body
	}

	// Sync all three runs to the library in one spool pass (the cross-run path).
	report, err := eng.SyncTo("", "lib", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("sync disk->lib: %v", err)
	}
	if report.Copied() != 3 {
		t.Fatalf("copied = %d, want 3", report.Copied())
	}

	// The slots' copies are spread across more than one tape — the drives were used across slots,
	// and each archive filed under its own slot (not a shared sync id).
	vols := map[string]bool{}
	for _, id := range ids {
		on := false
		for _, p := range eng.cat.Placements(id) {
			if p.Medium == "lib" {
				on = true
				for _, v := range p.Labels() {
					vols[v] = true
				}
			}
		}
		if !on {
			t.Fatalf("slot %s not recorded on the library after sync", id)
		}
	}
	if len(vols) < 2 {
		t.Fatalf("expected the sync to use >= 2 tapes (drives), got %d: %v", len(vols), vols)
	}

	// Drop the disk copies so restore reads the library, each slot byte-for-byte.
	name := config.DLE{Host: "localhost", Path: src}.Name()
	for _, id := range ids {
		removeRunFiles(t, eng, id)
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
// runs. A run's seal record lives only on the last tape of its span, so the head
// and middle tapes hold payload parts but no seal. The relabel guard must still
// refuse them: it judges protection from the catalog (which records a placement on
// every tape the run touches), not by scanning the mounted reel — scanning a
// sealless tape would find nothing and wrongly allow the wipe, destroying the
// spanned copy. --force remains the deliberate override.
func TestRelabelRefusesProtectedSpanTape(t *testing.T) {
	src := t.TempDir()
	// ~600 KiB across 256 KiB tapes: one run spans three bays.
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("x", 600*1024))

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Cycle:     "1d",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"lib":  {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "slots": "6", "volume_size": "262144"}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	day := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	s, err := eng.Run(context.Background(), day, nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "1", false, nil); err != nil {
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
		t.Fatalf("run did not span multiple tapes: %v", spanVols)
	}
	head := spanVols[0]

	// now is inside the run's minimum-age window, so the run is protected. Under the
	// old reel-scanning guard the head tape looked empty and would be relabeled.
	if err := eng.LoadVolume("lib", head, true, nil); err != nil {
		t.Fatalf("load head tape %s: %v", head, err)
	}
	err = eng.LabelVolume("lib", head, true, false, day, nil)
	if err == nil {
		t.Fatalf("relabel of head span tape %s must be refused while the run is protected", head)
	}
	if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("want a protected-run refusal, got: %v", err)
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

// TestSyncRunOutOfTapes checks that a run too big to fit even by spanning every
// available tape fails with an actionable "no further writable bay" error and is not
// recorded — the orphaned, unsealed parts left behind are reclaimable by relabel.
func TestSyncRunOutOfTapes(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), strings.Repeat("z", 200*1024)) // ~200 KiB

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// One 64 KiB bay: far too small for the run above, with no second bay to
			// span onto.
			"lib": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "slots": "1", "volume_size": "65536"}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := eng.LoadVolume("lib", "1", false, nil); err != nil {
		t.Fatalf("load bay-01: %v", err)
	}

	_, err = eng.SyncTo("", "lib", SyncSelection{}, true, false, nil)
	if err == nil {
		t.Fatal("expected sync of an oversized run to fail")
	}
	if !strings.Contains(err.Error(), "no further writable bay") {
		t.Fatalf("unexpected error: %v", err)
	}
	if eng.placedOn(s.ID, "lib") {
		t.Fatal("oversized run must not be recorded on the library")
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

// TestSyncResumesPartialTargetCopy locks the archive-granular presence rule: a run
// whose target copy holds only SOME of the archives the source copy holds is still in
// the backlog (never "up to date"), and the re-sync copies exactly the missing
// archives — leaving the already-committed ones untouched.
func TestSyncResumesPartialTargetCopy(t *testing.T) {
	srcA, srcB := t.TempDir(), t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), "alpha")
	write(t, filepath.Join(srcB, "b.txt"), "bravo")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: srcA}, {Host: "localhost", Path: srcB}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if len(s.Archives) != 2 {
		t.Fatalf("run has %d archives, want 2", len(s.Archives))
	}
	if _, err := eng.SyncTo("", "archive", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Simulate an interrupted copy: drop one archive from the target placement (the
	// same partial state a mid-run failure leaves — each archive records its placement
	// as it commits, so the failed one is simply absent).
	gone, kept := s.Archives[0], s.Archives[1]
	if _, _, err := eng.cat.RemoveArchive(s.ID, "archive", gone.DLE); err != nil {
		t.Fatal(err)
	}
	keptPosBefore := archivePosOn(t, eng, s.ID, "archive", kept.DLE)

	// The run must be back in the backlog, sized to the one missing archive.
	report, err := eng.SyncTo("", "archive", SyncSelection{}, false, false, nil)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("backlog = %d item(s), want 1 (partial copy must not read as up to date)", len(report.Items))
	}
	if it := report.Items[0]; it.RunID != s.ID || it.Archives != 1 || it.Bytes != gone.Compressed {
		t.Fatalf("backlog item = %+v, want run %s with 1 archive of %d bytes", it, s.ID, gone.Compressed)
	}

	// The re-sync completes the copy — copying only the missing archive.
	report, err = eng.SyncTo("", "archive", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("resume sync: %v", err)
	}
	if report.Copied() != 1 {
		t.Fatalf("copied = %d, want 1", report.Copied())
	}
	p, ok := placementOf(eng, s.ID, "archive")
	if !ok || !p.Holds(gone.DLE, gone.Level) || !p.Holds(kept.DLE, kept.Level) {
		t.Fatalf("target copy incomplete after resume: %+v", p)
	}
	if got := archivePosOn(t, eng, s.ID, "archive", kept.DLE); got.Commit != keptPosBefore.Commit {
		t.Fatalf("resume re-copied the already-present archive: commit %+v -> %+v", keptPosBefore.Commit, got.Commit)
	}

	// And now the mirror is genuinely complete.
	report, err = eng.SyncTo("", "archive", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if len(report.Items) != 0 {
		t.Fatalf("backlog = %d, want 0 (up to date)", len(report.Items))
	}
}

// placementOf returns a run's copy on a medium.
func placementOf(eng *Engine, runID, medium string) (catalog.Placement, bool) {
	for _, p := range eng.cat.Placements(runID) {
		if p.Medium == medium {
			return p, true
		}
	}
	return catalog.Placement{}, false
}

// archivePosOn returns one archive's recorded position on a run's copy on a medium.
func archivePosOn(t *testing.T, eng *Engine, runID, medium, dle string) catalog.ArchivePos {
	t.Helper()
	p, ok := placementOf(eng, runID, medium)
	if !ok {
		t.Fatalf("run %s has no copy on %q", runID, medium)
	}
	for _, a := range p.Archives {
		if a.DLE == dle {
			return a
		}
	}
	t.Fatalf("archive %s not on %q copy of %s", dle, medium, runID)
	return catalog.ArchivePos{}
}

// TestSyncStopsAfterFirstSinkError locks the fail-fast contract: a hard sink error
// (target unwritable) stops the sync at the first failing archive instead of
// attempting every remaining run and reporting the same error once per archive.
func TestSyncStopsAfterFirstSinkError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission failures cannot be simulated")
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "x")
	targetPath := t.TempDir()

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"archive": {Type: "disk", Params: map[string]string{"path": targetPath}},
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
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}
	for d := 21; d <= 22; d++ {
		if _, err := eng.Run(context.Background(), time.Date(2026, 6, d, 0, 0, 0, 0, time.UTC), nil); err != nil {
			t.Fatalf("dump %d: %v", d, err)
		}
	}

	// Make the target unwritable so every archive copy would fail the same way.
	runsDir := filepath.Join(targetPath, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runsDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runsDir, 0o755) })

	report, err := eng.SyncTo("", "archive", SyncSelection{}, true, false, nil)
	if err == nil {
		t.Fatal("sync to an unwritable target must fail")
	}
	if got := strings.Count(err.Error(), "permission denied"); got != 1 {
		t.Fatalf("error reports %d failures, want 1 (stop at the first hard sink error): %v", got, err)
	}
	if report.Copied() != 0 {
		t.Fatalf("copied = %d, want 0", report.Copied())
	}
}

// TestSyncTapeMidRunFailureResumes is the end-to-end regression for the "failed tape
// sync reads as up to date" bug: syncing two runs onto a one-tape pool fails mid-run
// when the tape fills and no writable bay remains. That failure must (a) report the
// run that DID complete as copied, (b) leave the target's partial copy resumable —
// never counted present — with label-correct positions, (c) keep a retry from
// touching the blank reels or claiming "up to date", and (d) once a second tape is
// labeled, copy exactly the missing archive so verify goes green and a further sync
// reports a genuinely complete mirror.
func TestSyncTapeMidRunFailureResumes(t *testing.T) {
	srcA, srcB := t.TempDir(), t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), strings.Repeat("a", 5*1024))
	write(t, filepath.Join(srcB, "b.txt"), strings.Repeat("b", 5*1024))
	tapeDir := t.TempDir()

	cfg := &config.Config{
		Landing: "disk",
		// One-day cycle: every run is a full, so the second run's payload sizes are
		// deterministic (no incremental change-detection races).
		Cycle: "1d",
		Media: map[string]config.Media{
			"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			// Three bays, 512 KB reels, no auto_label: with one labeled tape the first
			// run's copy fits, the second run's first archive fits, and its second
			// archive needs a roll that finds no writable bay — the mid-run failure.
			"tapes": {Type: "tape", Params: map[string]string{"dir": tapeDir, "slots": "3", "volume_size": "512000"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: srcA}, {Host: "localhost", Path: srcB}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skipf("GNU tar not available")
	}

	s1, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 1: %v", err)
	}
	// Grow one DLE so the second run cannot fit alongside the first on one reel.
	write(t, filepath.Join(srcA, "big.bin"), strings.Repeat("x", 100*1024))
	s2, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}

	if err := eng.LabelVolume("tapes", "T-1", false, false, time.Date(2026, 6, 22, 1, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("label T-1: %v", err)
	}

	// The sync fails mid-run: run 1 mirrors whole, run 2 lands only one of its two
	// archives before the roll finds no writable bay.
	report, err := eng.SyncTo("", "tapes", SyncSelection{}, true, false, nil)
	if err == nil {
		t.Fatal("sync onto a single filling tape should fail (no writable bay for the roll)")
	}
	if report.Copied() != 1 {
		t.Fatalf("copied = %d, want 1 (run 1 completed before the failure)", report.Copied())
	}
	p1, ok := placementOf(eng, s1.ID, "tapes")
	if !ok || len(p1.Archives) != len(s1.Archives) {
		t.Fatalf("run 1 should be whole on tapes, got %+v", p1)
	}
	p2, ok := placementOf(eng, s2.ID, "tapes")
	if !ok || len(p2.Archives) != 1 {
		t.Fatalf("run 2 should have exactly its committed archive on tapes, got %+v (archives=%d)", p2, len(p2.Archives))
	}
	assertLabeledPositions(t, p1, "T-1")
	assertLabeledPositions(t, p2, "T-1")
	partialPos := p2.Archives[0]

	// The retry must NOT report up to date — the missing archive is in the backlog —
	// and, still having no writable bay, must fail without touching the blank reels
	// or the recorded placements.
	dry, err := eng.SyncTo("", "tapes", SyncSelection{}, false, false, nil)
	if err != nil {
		t.Fatalf("dry-run after failure: %v", err)
	}
	if len(dry.Items) != 1 || dry.Items[0].RunID != s2.ID || dry.Items[0].Archives != 1 {
		t.Fatalf("backlog after failed sync = %+v, want run %s with 1 missing archive", dry.Items, s2.ID)
	}
	if _, err := eng.SyncTo("", "tapes", SyncSelection{}, true, false, nil); err == nil {
		t.Fatal("retry without a writable bay should still fail, not silently succeed")
	}
	if p, _ := placementOf(eng, s2.ID, "tapes"); len(p.Archives) != 1 {
		t.Fatalf("failed retry changed the target placement: %+v", p)
	}
	if n := tapesWithFiles(t, tapeDir); n != 1 {
		t.Fatalf("%d cartridges hold files, want 1 — a failed roll must never write an unverified/blank reel", n)
	}

	// Label a second tape; the retry now copies exactly the missing archive.
	if err := eng.LabelVolume("tapes", "T-2", false, false, time.Date(2026, 6, 22, 2, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("label T-2: %v", err)
	}
	report, err = eng.SyncTo("", "tapes", SyncSelection{}, true, false, nil)
	if err != nil {
		t.Fatalf("resume sync after labeling T-2: %v", err)
	}
	if report.Copied() != 1 {
		t.Fatalf("resume copied = %d run(s), want 1", report.Copied())
	}
	p2, ok = placementOf(eng, s2.ID, "tapes")
	if !ok || len(p2.Archives) != len(s2.Archives) {
		t.Fatalf("run 2 still incomplete on tapes after resume: %+v", p2)
	}
	assertLabeledPositions(t, p2, "T-1", "T-2")
	for _, a := range p2.Archives {
		if a.DLE == partialPos.DLE && a.Level == partialPos.Level && a.Commit != partialPos.Commit {
			t.Fatalf("resume re-copied the archive already on tape: commit %+v -> %+v", partialPos.Commit, a.Commit)
		}
	}

	// The copies must actually read back at their recorded positions.
	vr, err := eng.Verify(nil, VerifyOptions{Medium: "tapes"}, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vr.Failures != 0 {
		t.Fatalf("verify on tapes reports %d failure(s): %+v", vr.Failures, vr.Runs)
	}

	// And the mirror is now genuinely complete.
	dry, err = eng.SyncTo("", "tapes", SyncSelection{}, false, false, nil)
	if err != nil {
		t.Fatalf("final dry-run: %v", err)
	}
	if len(dry.Items) != 0 {
		t.Fatalf("mirror should be up to date, backlog = %+v", dry.Items)
	}
}

// assertLabeledPositions checks every recorded position of a tape placement names one
// of the allowed volume labels — never an empty or stale label (the wrong-position
// symptom of writing an unverified reel).
func assertLabeledPositions(t *testing.T, p catalog.Placement, allowed ...string) {
	t.Helper()
	ok := func(l string) bool {
		for _, a := range allowed {
			if l == a {
				return true
			}
		}
		return false
	}
	for _, a := range p.Archives {
		for _, pt := range a.Parts {
			if !ok(pt.Label) {
				t.Fatalf("archive %s part on volume %q, want one of %v", a.DLE, pt.Label, allowed)
			}
		}
		if !ok(a.Commit.Label) {
			t.Fatalf("archive %s commit on volume %q, want one of %v", a.DLE, a.Commit.Label, allowed)
		}
		if a.Index != (catalog.FilePos{}) && !ok(a.Index.Label) {
			t.Fatalf("archive %s index on volume %q, want one of %v", a.DLE, a.Index.Label, allowed)
		}
	}
}

// tapesWithFiles counts the emulated cartridges (slot directories) holding any file.
func tapesWithFiles(t *testing.T, tapeDir string) int {
	t.Helper()
	entries, err := os.ReadDir(tapeDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "slot-") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(tapeDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) > 0 {
			n++
		}
	}
	return n
}
