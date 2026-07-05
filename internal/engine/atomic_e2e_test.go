package engine

import (
	"context"

	"github.com/Niloen/nbackup/internal/archiveio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/transform"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// The atomic e2e runs with gzip standing in for the seal (registered as a PerFrame
// scheme), so the whole FRAMED-ATOMIC path — resolver, atom packing, whole-atom
// placement, per-atom decode — runs in CI without gpg. Every atom is one complete
// gzip message exactly as it would be one complete gpg message.
func init() {
	crypt.RegisterScheme(transform.Scheme[crypt.Options]{
		Name:    "testseal",
		Concat:  transform.ConcatPerFrame,
		Forward: func(crypt.Options) []string { return []string{"gzip", "-c"} },
		Reverse: func(crypt.Options) []string { return []string{"gzip", "-d", "-c"} },
	})
}

// incompressible fills a file with LCG bytes so the inner frames stay near frame_size
// and the atom bound genuinely cuts.
func incompressible(t *testing.T, path string, n int) {
	t.Helper()
	b := make([]byte, n)
	x := uint32(99991)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAtomicEndToEnd drives the FRAMED-ATOMIC shape through the real engine on disk:
// dump (multi-atom archive with RawSize'd seals), deep verify (per-atom structural
// decode), whole-DLE restore, selected-file recovery, an atom-carrying copy whose
// seals match the source bit-exactly, and the sample drill's key-proving atom sample.
func TestAtomicEndToEnd(t *testing.T) {
	src := t.TempDir()
	diskDir := t.TempDir()
	secondDir := t.TempDir()

	incompressible(t, filepath.Join(src, "big.bin"), 300*1024)
	write(t, filepath.Join(src, "etc", "hosts"), "127.0.0.1 localhost\n")
	write(t, filepath.Join(src, "tail.txt"), "the needle\n")

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":   {Type: "disk", Params: map[string]string{"path": diskDir}},
			"second": {Type: "disk", Params: map[string]string{"path": secondDir}},
		},
		Sources:   []config.DLE{{Host: "localhost", Path: src}},
		Workdir:   t.TempDir(),
		StateDir:  t.TempDir(),
		PartSize:  "64KiB", // atom bound: several atoms over ~300 KiB of incompressible input
		FrameSize: "16KiB",
	}
	cfg.Compress.Scheme = "gzip"
	cfg.Encrypt.Scheme = "testseal"

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

	// The committed archive is atomic: several parts, each a whole sealed atom with a
	// RawSize'd seal (the shape's member→atom map).
	runs := eng.cat.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	runID := runs[0].ID
	s, err := eng.cat.ReadRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Archives[0]
	if a.Shape != record.ShapeAtomic {
		t.Fatalf("archive shape = %q, want %q", a.Shape, record.ShapeAtomic)
	}
	if a.Parts < 2 {
		t.Fatalf("archive has %d part(s), want several atoms", a.Parts)
	}
	seals, err := eng.fs.AtomSeals(archiveio.Ref{Run: runID, DLE: a.DLE, Level: a.Level})
	if err != nil || len(seals) != a.Parts {
		t.Fatalf("AtomSeals = %d (err %v), want %d", len(seals), err, a.Parts)
	}
	var rawSum int64
	for _, sl := range seals {
		if sl.RawSize <= 0 {
			t.Fatalf("seal missing RawSize: %+v", sl)
		}
		rawSum += sl.RawSize
	}
	if rawSum != a.Uncompressed {
		t.Fatalf("seal RawSize sum = %d, want the raw stream size %d", rawSum, a.Uncompressed)
	}

	// The atoms are honest files: part index BEFORE the extensions.
	names, _ := filepath.Glob(filepath.Join(diskDir, "runs", runID, "*.p000.tar.gz.testseal"))
	if len(names) != 1 {
		all, _ := filepath.Glob(filepath.Join(diskDir, "runs", runID, "*"))
		t.Fatalf("atom naming: want one *.p000.tar.gz.testseal (seal ext last), got %v", all)
	}

	// Deep verify decodes per atom and compares members+offsets.
	rep, err := eng.Verify(nil, VerifyOptions{Checks: CheckChecksum | CheckStructural}, logfDiscard)
	if err != nil || rep.Failures != 0 {
		t.Fatalf("deep verify: failures=%d err=%v", rep.Failures, err)
	}

	// Whole-DLE restore reproduces the tree.
	name := config.DLE{Host: "localhost", Path: src}.Name()
	dest := filepath.Join(t.TempDir(), "out")
	if err := eng.Restore(runID, name, dest, false, logfDiscard); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertContent(t, filepath.Join(dest, "tail.txt"), "the needle\n")

	// Selected-file recovery (the covering-atoms path, whole-stream fallback on a
	// non-ranged copy — either way the bytes must land).
	tree, err := eng.OpenRecover(name, "2026-06-21")
	if err != nil {
		t.Fatalf("OpenRecover: %v", err)
	}
	steps, err := tree.Collect([]string{"/etc/hosts"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	selDest := t.TempDir()
	if _, _, err := eng.ExtractSelection(steps, selDest, logfDiscard, nil); err != nil {
		t.Fatalf("ExtractSelection: %v", err)
	}
	assertContent(t, filepath.Join(selDest, "etc", "hosts"), "127.0.0.1 localhost\n")

	// A copy carries the atoms 1:1: the target's seals are the source's, bit-exact.
	if err := eng.CopyRun(runID, "", "second", false, logfDiscard); err != nil {
		t.Fatalf("copy: %v", err)
	}
	var secondSeals []record.PartSeal
	for _, p := range eng.cat.Placements(runID) {
		if p.Medium == "second" {
			pa, ok := p.Placed(a.DLE, a.Level)
			if !ok {
				t.Fatal("copy recorded no placed archive on the target")
			}
			secondSeals = pa.Seals
		}
	}
	if !reflect.DeepEqual(seals, secondSeals) {
		t.Fatalf("copied seals differ from the source's:\n src %+v\n tgt %+v", seals, secondSeals)
	}

	// The sample drill runs the key-proving atom sample (decrypt-and-list one atom).
	drep, err := eng.Drill(DrillOptions{Tier: drill.TierSample, Apply: true, Now: day1.Add(time.Hour)}, logfDiscard)
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if !drep.SLOMet() || len(drep.Targets) == 0 {
		t.Fatalf("drill sample on the atomic archive failed: %+v", drep.Targets)
	}
}

// TestAtomCeilingLadder pins the atom validation ladder's first two rungs: `nb check`
// warns about dumptype × medium pairs whose atoms exceed the medium's part ceiling,
// and the dump-time check hard-errors for the routed pair — a sealed atom cannot be
// re-cut to fit.
func TestAtomCeilingLadder(t *testing.T) {
	cfg := &config.Config{
		Landing: "offsite",
		Media: map[string]config.Media{
			"offsite": {Type: "cloud", Params: map[string]string{"url": "file://" + t.TempDir()}},
		},
		Sources:  []config.DLE{},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
		DumpTypes: map[string]config.DumpType{
			"vm": {PartSize: "100GiB", Encrypt: &config.EncryptConfig{Scheme: "testseal"}},
		},
	}
	cfg.Compress.Scheme = "gzip"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Check-time: a warning naming the pair, never a failure.
	rep := eng.Check(false)
	found := false
	for _, l := range rep.Server {
		if l.Warn && strings.Contains(l.Msg, "part ceiling") && strings.Contains(l.Msg, `"vm"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("check should warn about the vm × offsite ceiling pair; server lines: %+v", rep.Server)
	}
	// Dump-time: the routed pair is a hard error.
	if err := eng.atomCeilingErr("vm", cfg.AtomSizeBytes("vm")); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("dump-time ceiling check should hard-error, got: %v", err)
	}
	// A fitting atom size passes.
	if err := eng.atomCeilingErr("vm", 1<<30); err != nil {
		t.Fatalf("a 1GiB atom fits the cloud ceiling: %v", err)
	}
}
