package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/record"
)

// twoDLEFixture dumps two DLEs into one run on disk and mirrors the run onto an
// offsite disk medium, so a run has two archives across two copies — the shape the
// multi-copy verify matrix reasons over.
type twoDLEFixture struct {
	eng     *Engine
	runID   string
	dleA    string
	dleB    string
	diskDir string
	offsite string
}

func newTwoDLEFixture(t *testing.T) *twoDLEFixture {
	t.Helper()
	srcA, srcB := t.TempDir(), t.TempDir()
	write(t, filepath.Join(srcA, "a.txt"), "alpha")
	write(t, filepath.Join(srcB, "b.txt"), "bravo")
	diskDir, offsiteDir := t.TempDir(), t.TempDir()

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": diskDir}},
			"offsite": {Type: "disk", Params: map[string]string{"path": offsiteDir}},
		},
		Sources: []config.DLE{
			{Host: "localhost", Path: srcA},
			{Host: "localhost", Path: srcB},
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.tc.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	s, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if _, err := eng.SyncTo("", "offsite", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync offsite: %v", err)
	}
	return &twoDLEFixture{
		eng:     eng,
		runID:   s.ID,
		dleA:    config.DLE{Host: "localhost", Path: srcA}.Name(),
		dleB:    config.DLE{Host: "localhost", Path: srcB}.Name(),
		diskDir: diskDir,
		offsite: offsiteDir,
	}
}

// TestVerifyIntactCopyRemains exercises verify.go's "FAILED but an intact copy remains"
// branch: with a DLE on two media, corrupting one copy must report a failure that names
// the surviving copy to re-copy from, not a blanket all-copies failure.
func TestVerifyIntactCopyRemains(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng

	// Corrupt only the disk copy of the full; the offsite copy stays intact.
	corrupt(t, payloadFile(t, f.diskDir, "run-2026-06-21.000000", 0))

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{"run-2026-06-21.000000"}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("failures = %d, want 1 (one corrupt copy)", rep.Failures)
	}
	if !cap.contains("an intact copy remains on offsite") {
		t.Errorf("verify should reassure that an intact copy remains; log:\n%s", cap.joined())
	}
	if cap.contains("FAILED on all") {
		t.Errorf("with a surviving copy verify must not report all copies failed; log:\n%s", cap.joined())
	}
}

// TestVerifyMediumScopedRespectsArchiveGranularity locks the archive-granular placement
// rule under a pinned medium: an archive pruned from one copy (its parts removed from that
// placement) is by design absent there, not a missing-position fault. A medium-scoped
// verify judges the copy against what it actually records — so the pruned DLE is not failed
// on that copy (it survives, and verifies, on its other copy), and only the archives the
// copy still holds are checked.
func TestVerifyMediumScopedRespectsArchiveGranularity(t *testing.T) {
	f := newTwoDLEFixture(t)
	eng := f.eng

	// Prune DLE A's archive from the offsite copy only; disk still holds it, so the
	// run's medium-independent content still lists both archives.
	if _, _, err := eng.cat.RemoveArchive(f.runID, "offsite", f.dleA); err != nil {
		t.Fatal(err)
	}

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{f.runID}, VerifyOptions{Medium: "offsite"}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 0 {
		t.Fatalf("failures = %d, want 0 (a pruned archive is absent by design, not a fault)", rep.Failures)
	}
	if cap.contains("POSITION MISSING") {
		t.Errorf("a per-archive prune must not read as a missing position; log:\n%s", cap.joined())
	}
	// The archive the offsite copy still holds (DLE B) is verified there; DLE A is not
	// reported against the offsite copy at all.
	for i := range rep.Runs[0].Archives {
		if av := &rep.Runs[0].Archives[i]; av.DLE == f.dleA && av.Medium == "offsite" {
			t.Fatalf("pruned DLE A must not be judged on the offsite copy, got %+v", av)
		}
	}
}

// TestVerifySkipsCopyOnUnknownMedium exercises the ErrUnknownMedium branch: a run whose
// only copy lives on a medium this config no longer defines is out of scope, not damaged
// — verify must SKIP it (with a note) and never report a false integrity failure.
func TestVerifySkipsCopyOnUnknownMedium(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	id := "run-2026-06-21.000000"

	// Leave only the offsite copy, then drop offsite from the config so it is unknown.
	if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
		t.Fatal(err)
	}
	delete(eng.cfg.Media, "offsite")

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{id}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 0 {
		t.Fatalf("failures = %d, want 0 (a copy on an unknown medium is skipped, not failed)", rep.Failures)
	}
	if !cap.contains("SKIPPED — copies only on media not in this config") {
		t.Errorf("verify should announce the whole-run skip; log:\n%s", cap.joined())
	}
}

// TestVerifyCopyOpenErrorIsPipeline exercises the non-unknown copy-open-error branch: a
// copy on a medium that IS defined but cannot be opened is a real pipeline failure
// (badCopy, ClassPipeline), distinct from the out-of-scope unknown-medium skip.
func TestVerifyCopyOpenErrorIsPipeline(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	id := "run-2026-06-21.000000"

	// Leave only the offsite copy, then redefine offsite as an unreachable cloud medium
	// — still known to the config, so opening it is a genuine failure, not a skip.
	if _, err := eng.cat.RemovePlacement(id, "disk"); err != nil {
		t.Fatal(err)
	}
	eng.cfg.Media["offsite"] = config.Media{Type: "cloud", Params: map[string]string{"url": "bogusscheme://nope"}}

	cap := &capturingLogf{}
	rep, err := eng.Verify([]string{id}, VerifyOptions{}, cap.log)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("failures = %d, want 1 (a configured copy that will not open is a failure)", rep.Failures)
	}
	if cls := firstFailClass(rep); cls != drill.ClassPipeline {
		t.Fatalf("open-failure class = %s, want pipeline", cls)
	}
}

// TestVerifyDeepStructuralZeroChangeIncremental locks the fix for the false-integrity
// failure a zero-change incremental produced: a DLE unchanged since its base writes a
// payload + commit but no member index (file_count 0), yet GNU tar's incremental payload
// still carries a directory census (./, ./sub/). The structural check must treat the
// clean decode + `tar -t` as the whole proof and NOT compare that census to the empty
// recorded index — else it falsely reports the healthy archive as corrupt.
func TestVerifyDeepStructuralZeroChangeIncremental(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "full content")
	diskDir := t.TempDir()
	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": diskDir}}},
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
		t.Skip("GNU tar not available")
	}
	// Full, then a second run with the source untouched → a zero-change incremental.
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("full dump: %v", err)
	}
	zc, err := eng.Run(context.Background(), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("zero-change dump: %v", err)
	}
	// Confirm the second run really is a no-index zero-change incremental (file_count 0).
	var sawZeroChange bool
	for _, a := range zc.Archives {
		if a.Level >= 1 && a.FileCount == 0 {
			sawZeroChange = true
		}
	}
	if !sawZeroChange {
		t.Fatalf("expected a zero-change incremental (level>=1, file_count 0) in %s; got %+v", zc.ID, zc.Archives)
	}
	// Deep (checksum + structural) verify must pass for every run — the zero-change one
	// included; before the fix it reported a false integrity failure on that archive.
	rep, err := eng.Verify(nil, VerifyOptions{Checks: CheckChecksum | CheckStructural}, nil)
	if err != nil {
		t.Fatalf("deep verify: %v", err)
	}
	if rep.Failures != 0 {
		t.Fatalf("deep verify failures = %d, want 0 (zero-change incremental must not false-fail): %+v", rep.Failures, rep.Runs)
	}
}

// TestMembersDiffOffsets pins the offset-aware structural comparison: identical lists
// match; a member whose stream offset moved is a difference even when every name
// survives; and an unreported offset (-1) on either side skips the offset check rather
// than failing an archiver that cannot report offsets.
func TestMembersDiffOffsets(t *testing.T) {
	recorded := []record.Member{{Path: "./a", Off: 0}, {Path: "./b", Off: 1024}}
	if d := membersDiff(recorded, []record.Member{{Path: "./b", Off: 1024}, {Path: "./a", Off: 0}}); d != "" {
		t.Fatalf("equal sets must match regardless of order, got %q", d)
	}
	moved := []record.Member{{Path: "./a", Off: 0}, {Path: "./b", Off: 2048}}
	if d := membersDiff(recorded, moved); d == "" {
		t.Fatal("a member that moved in the stream must be reported")
	}
	unreported := []record.Member{{Path: "./a", Off: -1}, {Path: "./b", Off: -1}}
	if d := membersDiff(recorded, unreported); d != "" {
		t.Fatalf("unreported offsets must not fail the name-set match, got %q", d)
	}
	if d := membersDiff(recorded, []record.Member{{Path: "./a", Off: 0}, {Path: "./c", Off: 1024}}); d == "" {
		t.Fatal("a renamed member must be reported")
	}
}

// TestOldFormatIndexDegradesGracefully pins the pre-shapes compatibility contract:
// an archive whose per-archive index is the OLD format (a bare gzip JSON array of
// member paths) must never fail or be misreported. There is deliberately no migration
// (greenfield); the contract is graceful degradation — the archive stays restorable
// and deep-verifiable (the clean decode is the structural proof), browse is simply
// empty, and nothing errors out over an existing catalog/workdir.
func TestOldFormatIndexDegradesGracefully(t *testing.T) {
	src := t.TempDir()
	diskDir := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "v1")

	cfg := &config.Config{
		Landing:  "disk",
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": diskDir}}},
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
		t.Skip("GNU tar not available")
	}
	day1 := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if _, err := eng.Run(context.Background(), day1, nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Rewrite the on-medium index in the OLD format and poison the cache with it too.
	oldIndex := func() []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if err := json.NewEncoder(gz).Encode([]string{"./", "./keep.txt"}); err != nil {
			t.Fatal(err)
		}
		gz.Close()
		return buf.Bytes()
	}()
	onMedium, _ := filepath.Glob(filepath.Join(diskDir, "runs", "*", "*-index.json.gz"))
	if len(onMedium) != 1 {
		t.Fatalf("want one on-medium index, got %v", onMedium)
	}
	if err := os.Chmod(onMedium[0], 0o644); err != nil { // committed payloads are 0444
		t.Fatal(err)
	}
	if err := os.WriteFile(onMedium[0], oldIndex, 0o644); err != nil {
		t.Fatal(err)
	}
	cached, _ := filepath.Glob(filepath.Join(cfg.Workdir, "member-index", "*.idx.gz"))
	for _, c := range cached {
		if err := os.WriteFile(c, oldIndex, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Deep verify passes: the decode+list is the structural proof; the unreadable old
	// index must not surface as an error, and never as a false integrity verdict.
	rep, err := eng.Verify(nil, VerifyOptions{Checks: CheckChecksum | CheckStructural}, logfDiscard)
	if err != nil || rep.Failures != 0 {
		t.Fatalf("deep verify over an old-format index: failures=%d err=%v report=%+v", rep.Failures, err, rep.Runs)
	}

	// Browse degrades to an empty tree — no error, nothing selectable.
	name := config.DLE{Host: "localhost", Path: src}.Name()
	tree, err := eng.OpenRecover(name, "2026-06-21")
	if err != nil {
		t.Fatalf("OpenRecover must not error over an old-format index: %v", err)
	}
	if _, ok := tree.Lookup("keep.txt"); ok {
		t.Fatal("old-format members are not migrated; the browse tree should be empty")
	}

	// The whole-DLE restore never needed the index: still fully functional.
	runID := eng.cat.Runs()[0].ID
	dest := filepath.Join(t.TempDir(), "out")
	if err := eng.Restore(runID, name, dest, false, logfDiscard); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "keep.txt"), "v1")
}
