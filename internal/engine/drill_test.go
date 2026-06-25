package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/drill"
)

// drillFixture builds a disk-landing engine with an offsite disk medium and a single
// DLE, runs a full and (the next day) an incremental, and mirrors both slots offsite.
// It returns the engine, the DLE name, the source dir, and each medium's path.
type drillFixture struct {
	eng        *Engine
	dle, src   string
	diskDir    string
	offsiteDir string
}

func newDrillFixture(t *testing.T, codec string) *drillFixture {
	t.Helper()
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "full content")
	diskDir, offsiteDir := t.TempDir(), t.TempDir()

	cfg := &config.Config{
		Landing: "disk",
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": diskDir}},
			"offsite": {Type: "disk", Params: map[string]string{"path": offsiteDir}},
		},
		Sources: []config.DLE{{Host: "localhost", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = codec

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}

	if _, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("full dump: %v", err)
	}
	write(t, filepath.Join(src, "b.txt"), "incremental content")
	if _, err := eng.Run(time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("incremental dump: %v", err)
	}
	if _, err := eng.SyncTo("", "offsite", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync offsite: %v", err)
	}
	return &drillFixture{eng: eng, dle: config.DLE{Host: "localhost", Path: src}.Name(), src: src, diskDir: diskDir, offsiteDir: offsiteDir}
}

// payloadFile locates a slot's archive payload (not the .hdr sidecar) for a level on
// a disk medium, so a test can corrupt it.
func payloadFile(t *testing.T, mediumDir, slotID string, level int) string {
	t.Helper()
	all, _ := filepath.Glob(filepath.Join(mediumDir, "slots", slotID, "*"))
	for _, f := range all {
		base := filepath.Base(f)
		if strings.Contains(base, fmt.Sprintf("-L%d.", level)) && !strings.HasSuffix(base, ".hdr") {
			return f
		}
	}
	t.Fatalf("no payload for %s L%d under %s (have %v)", slotID, level, mediumDir, all)
	return ""
}

// TestVerifyDeepStructural exercises the structural (`--deep`) verify primitive: it
// passes on a healthy slot (the pipeline completes and members match the seal), and a
// payload corruption is caught — as an integrity fault at the checksum check, and as a
// pipeline fault at the structural check when the codec can no longer decode it.
func TestVerifyDeepStructural(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng

	// Healthy: deep verify (checksum + structural) passes for both copies.
	rep, err := eng.Verify(nil, VerifyOptions{Checks: CheckChecksum | CheckStructural}, nil)
	if err != nil {
		t.Fatalf("deep verify: %v", err)
	}
	if rep.Failures != 0 {
		t.Fatalf("healthy deep verify failures = %d, want 0", rep.Failures)
	}
	// Every archive verdict carries a structural-checked OK.
	for _, sv := range rep.Slots {
		for _, av := range sv.Archives {
			if !av.OK {
				t.Fatalf("archive %s %s L%d not OK: %s", av.Slot, av.DLE, av.Level, av.Detail)
			}
		}
	}

	// Corrupt the full payload on the offsite copy: checksum verify scoped to offsite
	// reports an integrity fault.
	corrupt(t, payloadFile(t, f.offsiteDir, "slot-2026-06-21", 0))
	rep, err = eng.Verify([]string{"slot-2026-06-21"}, VerifyOptions{Checks: CheckChecksum, Medium: "offsite"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("corrupt checksum failures = %d, want 1", rep.Failures)
	}
	if cls := firstFailClass(rep); cls != drill.ClassIntegrity {
		t.Fatalf("corrupt checksum class = %s, want integrity", cls)
	}
}

// TestVerifyStructuralPipelineFault confirms that, with a real codec, a payload that
// no longer decompresses is classified as a pipeline fault (key/codec drift) by the
// structural check — the failure mode checksum-only verify reports merely as integrity.
func TestVerifyStructuralPipelineFault(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available")
	}
	f := newDrillFixture(t, "gzip")
	eng := f.eng

	// Garble the middle of the gzip payload so decompression fails outright.
	corrupt(t, payloadFile(t, f.diskDir, "slot-2026-06-21", 0))
	rep, err := eng.Verify([]string{"slot-2026-06-21"}, VerifyOptions{Checks: CheckStructural, Medium: "disk"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 {
		t.Fatalf("structural failures = %d, want 1", rep.Failures)
	}
	if cls := firstFailClass(rep); cls != drill.ClassPipeline {
		t.Fatalf("garbled-codec class = %s, want pipeline", cls)
	}
}

// TestDrillTiersPass runs each drill tier against a healthy chain and asserts it
// passes, writes a ledger, and (dry-run) writes nothing.
func TestDrillTiersPass(t *testing.T) {
	for _, tier := range []drill.Tier{drill.TierChecksum, drill.TierStructural, drill.TierChain, drill.TierStock} {
		t.Run(tier.String(), func(t *testing.T) {
			f := newDrillFixture(t, "none")
			eng := f.eng
			now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)

			// Dry-run: reports the target, writes no ledger.
			rep, err := eng.Drill(DrillOptions{Tier: tier, Sample: 1, Window: 30 * 24 * time.Hour, Now: now}, nil)
			if err != nil {
				t.Fatalf("dry-run drill: %v", err)
			}
			if len(rep.Targets) != 1 || rep.Targets[0].DLE != f.dle {
				t.Fatalf("dry-run targets = %+v", rep.Targets)
			}
			if _, err := os.Stat(filepath.Join(eng.cfg.WorkdirPath(), drill.LedgerFile)); !os.IsNotExist(err) {
				t.Fatalf("dry-run must not write the ledger")
			}

			// Apply: drills the target successfully and records it.
			rep, err = eng.Drill(DrillOptions{Tier: tier, Sample: 1, Window: 30 * 24 * time.Hour, Apply: true, Worm: true, Now: now}, nil)
			if err != nil {
				t.Fatalf("apply drill: %v", err)
			}
			if rep.Failures != 0 {
				t.Fatalf("tier %s failures = %d (%+v)", tier, rep.Failures, rep.Targets)
			}
			led, err := drill.Load(eng.cfg.WorkdirPath())
			if err != nil {
				t.Fatal(err)
			}
			rec, ok := led.Get(f.dle)
			if !ok || !rec.OK || rec.Tier != tier.String() {
				t.Fatalf("ledger record after drill = %+v ok=%v", rec, ok)
			}
		})
	}
}

// TestDrillMissingCopy classifies a drill against a medium that lacks the slot as a
// missing-copy fault.
func TestDrillMissingCopy(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	// Drop the offsite copy of the full so the chain is incomplete on offsite.
	if _, err := eng.cat.RemovePlacement("slot-2026-06-21", "offsite"); err != nil {
		t.Fatal(err)
	}
	rep, err := eng.Drill(DrillOptions{Tier: drill.TierStructural, Medium: "offsite", Sample: 1, Apply: true, Now: time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 || rep.Targets[0].Class != drill.ClassMissing {
		t.Fatalf("missing-copy drill = %+v (failures %d)", rep.Targets, rep.Failures)
	}
}

// TestDrillBrokenChain classifies a chain whose incremental no longer untars as a
// chain-composition fault.
func TestDrillBrokenChain(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng
	// Garble the incremental payload on disk: the bytes are not a tar stream, so the
	// chain restore (full then incremental) fails composing — a chain fault.
	corrupt(t, payloadFile(t, f.diskDir, "slot-2026-06-22", 1))
	rep, err := eng.Drill(DrillOptions{Tier: drill.TierChain, Medium: "disk", Sample: 1, Apply: true, Now: time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failures != 1 || rep.Targets[0].Class != drill.ClassChain {
		t.Fatalf("broken-chain drill = %+v (failures %d)", rep.Targets, rep.Failures)
	}
}

// TestDrillWormProbeMutableDisk confirms the WORM probe reports a disk medium as
// mutable (its probe object deletes successfully) and reuses a single probe object.
func TestDrillWormProbeMutableDisk(t *testing.T) {
	f := newDrillFixture(t, "none")
	eng := f.eng

	res := eng.wormProbe("disk", true, time.Now().UTC())
	if !res.Tested || res.Enforced {
		t.Fatalf("disk worm probe = %+v, want tested & not enforced (mutable)", res)
	}
	if !strings.Contains(res.Detail, "MUTABLE") {
		t.Fatalf("worm detail = %q", res.Detail)
	}
	// A second probe also succeeds and never accumulates more than one probe object.
	_ = eng.wormProbe("disk", true, time.Now().UTC())
	probes, _ := filepath.Glob(filepath.Join(f.diskDir, "slots", wormProbeSlot))
	if len(probes) > 1 {
		t.Fatalf("worm probe accumulated %d objects, want <=1", len(probes))
	}
}

// TestDrillUnattendedSkipsSwap verifies that, in unattended (cron) mode, a target
// whose only drilled copy sits on a single-drive station with the wrong reel loaded
// is skipped (a non-failing SLO warning), so the run never blocks and exits clean.
func TestDrillUnattendedSkipsSwap(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "tape me")

	cfg := &config.Config{
		Landing:   "disk",
		AutoLabel: true,
		Media: map[string]config.Media{
			"disk":    {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"station": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "mode": "manual", "reels": "3", "volume_size": "1048576"}},
		},
		Sources: []config.DLE{{Host: "localhost", Path: src}},
		Workdir: t.TempDir(),
	}
	cfg.Compress.Codec = "none"

	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := eng.archiverFor(config.DefaultDumpType, ""); err != nil || m.Check() != nil {
		t.Skip("GNU tar not available")
	}
	if _, err := eng.Run(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}
	// Load a blank reel and mirror the slot onto the station (auto-labeled).
	if err := eng.LoadVolume("station", "reel-01", false, nil); err != nil {
		t.Fatalf("load reel-01: %v", err)
	}
	if _, err := eng.SyncTo("", "station", SyncSelection{}, true, false, nil); err != nil {
		t.Fatalf("sync to station: %v", err)
	}
	// Swap in a different blank reel, so the slot's reel is no longer in the drive.
	if err := eng.LoadVolume("station", "reel-02", false, nil); err != nil {
		t.Fatalf("load reel-02: %v", err)
	}

	// Unattended drill of the station copy: it must SKIP (not fail) and exit clean.
	rep, err := eng.Drill(DrillOptions{Tier: drill.TierStructural, Medium: "station", Sample: 1, Unattended: true, Apply: true, Now: time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatalf("unattended drill errored instead of skipping: %v", err)
	}
	if rep.Failures != 0 {
		t.Fatalf("unattended drill failures = %d, want 0 (skip is not a failure)", rep.Failures)
	}
	if rep.Skipped != 1 || rep.Targets[0].Class != drill.ClassSkipped {
		t.Fatalf("unattended drill = %+v, want 1 skipped", rep.Targets)
	}
}

// TestDrillPostureAudit checks the 3-2-1-1-0 posture audit names the expected checks.
func TestDrillPostureAudit(t *testing.T) {
	f := newDrillFixture(t, "none")
	rep, err := f.eng.Drill(DrillOptions{Tier: drill.TierStructural, Sample: 1, Apply: true, Worm: true, Now: time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"3 copies", "2 media", "1 offsite", "1 immutable", "0 errors"}
	got := map[string]bool{}
	for _, c := range rep.Posture.Checks {
		got[c.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("posture missing check %q (have %v)", w, got)
		}
	}
	// Two media hold copies (disk + offsite) and offsite is present.
	if rep.Posture.Media < 2 || !rep.Posture.Offsite {
		t.Fatalf("posture media=%d offsite=%v, want >=2 and offsite", rep.Posture.Media, rep.Posture.Offsite)
	}
}

// corrupt overwrites a payload with garbage of the same length, so its checksum
// fails and neither the codec nor tar can decode it (a fatal error, not a warning).
func corrupt(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatalf("payload %s is empty; cannot corrupt", path)
	}
	garbage := make([]byte, info.Size())
	for i := range garbage {
		garbage[i] = 0xAA
	}
	if err := os.WriteFile(path, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
}

// firstFailClass returns the class of the first failing archive verdict in a report.
func firstFailClass(rep *VerifyReport) drill.Class {
	for _, sv := range rep.Slots {
		for _, av := range sv.Archives {
			if !av.OK {
				return av.Class
			}
		}
	}
	return drill.ClassNone
}
