package engine

import (
	"context"
	"errors"
	"github.com/Niloen/nbackup/internal/archiveio"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// refuseDeleteVolume wraps a disk volume but refuses every RemoveFile — the behaviour of
// an Object-Lock / immutable bucket. It lets the WORM probe's enforced branch be tested
// without real cloud immutability. Registered as the "wormlock" medium type below.
type refuseDeleteVolume struct{ media.Volume }

func (refuseDeleteVolume) RemoveFile(pos int) error {
	return errors.New("object-lock: delete of a committed object is denied")
}

func init() {
	media.Register(media.Spec{
		Type: "wormlock",
		New: func(opts media.Options, _ string) (media.Volume, error) {
			v, err := media.OpenVolume("disk", opts, "")
			if err != nil {
				return nil, err
			}
			return refuseDeleteVolume{v}, nil
		},
		Params:          []string{"path"},
		ConcurrentWrite: true,
	})
}

// TestWormProbeEnforcedRefusedDelete exercises posture.go's enforced/delete-refused
// branch: an address-identified medium whose delete is refused proves immutability is
// ENFORCED — the probe object persists as the proof.
func TestWormProbeEnforcedRefusedDelete(t *testing.T) {
	cfg := &config.Config{
		Landing:  config.MediumList{"wormlock"},
		Media:    map[string]config.Media{"wormlock": {Type: "wormlock", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	res := eng.drl.wormProbe("wormlock", true, time.Now().UTC())
	if !res.Tested {
		t.Fatalf("an address-identified medium should be actively probed: %+v", res)
	}
	if !res.Enforced {
		t.Fatalf("a refused delete must report immutability ENFORCED: %+v", res)
	}
	if !strings.Contains(res.Detail, "ENFORCED") {
		t.Errorf("detail = %q, want it to say ENFORCED", res.Detail)
	}
}

// TestWormProbeAppendOnlyMedium exercises the append-only branch: a labeled (tape)
// medium is immutable by construction and reported enforced without writing a probe.
func TestWormProbeAppendOnlyMedium(t *testing.T) {
	cfg := &config.Config{
		Landing: config.MediumList{"disk"},
		Media: map[string]config.Media{
			"disk":  {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"vault": {Type: "tape", Params: map[string]string{"dir": t.TempDir(), "slots": "2"}},
		},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	res := eng.drl.wormProbe("vault", true, time.Now().UTC())
	if res.Tested {
		t.Errorf("an append-only medium must not have a probe written: %+v", res)
	}
	if !res.Enforced || !strings.Contains(res.Detail, "append-only") {
		t.Fatalf("append-only medium should report enforced immutability: %+v", res)
	}
}

// postureEngine builds a disk-landing engine with no runs — the empty-catalog baseline
// the degraded posture checks reason over.
func postureEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
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
	return eng
}

// TestPostureDegradedNoCopies exercises the failing/degraded posture branches: with no
// backup copies recorded, "3 copies" FAILs, "2 media"/"1 offsite"/"1 immutable" WARN, and
// a nonzero drill-failure count FAILs "0 errors".
func TestPostureDegradedNoCopies(t *testing.T) {
	eng := postureEngine(t)

	p := eng.drl.posture(WormResult{}, 1) // 1 drill failure this run
	want := map[string]PostureStatus{
		"3 copies":    PostureFail,
		"2 media":     PostureWarn,
		"1 offsite":   PostureWarn,
		"1 immutable": PostureWarn,
		"0 errors":    PostureFail,
	}
	statuses := map[string]PostureStatus{}
	for _, c := range p.Checks {
		statuses[c.Name] = c.Status
	}
	for name, st := range want {
		if statuses[name] != st {
			t.Errorf("check %q = %v, want %v", name, statuses[name], st)
		}
	}
}

// TestPostureViewOffline checks the webui's offline audit: it carries the 3-2-1 core
// and "0 errors"/key/capacity, reports "1 immutable" as INFO (it cannot probe a
// medium), and — the offline contract — omits the incremental-state digit (a remote
// DLE's .snar lives host-side) so a browser hitting /drills never opens a connection.
func TestPostureViewOffline(t *testing.T) {
	eng := postureEngine(t)

	p := eng.PostureView(2) // 2 failing drills feed "0 errors"
	statuses := map[string]PostureStatus{}
	for _, c := range p.Checks {
		statuses[c.Name] = c.Status
	}
	want := map[string]PostureStatus{
		"3 copies":      PostureFail, // no dump run: no backup copy
		"2 media":       PostureWarn,
		"1 offsite":     PostureWarn,
		"1 immutable":   PostureInfo, // not probed offline
		"0 errors":      PostureFail, // 2 failing drills
		"key reachable": PostureOK,
		"capacity OK":   PostureOK,
	}
	for name, st := range want {
		if statuses[name] != st {
			t.Errorf("check %q = %v, want %v", name, statuses[name], st)
		}
	}
	if _, ok := statuses["incremental state present"]; ok {
		t.Errorf("offline PostureView must omit the host-probing incremental-state digit")
	}
}

// TestPostureSingleCopyWarns exercises the 1-backup-copy degraded branch: a single dump
// leaves one placement, so "3 copies" warns that 3-2-1 wants a second backup.
func TestPostureSingleCopyWarns(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "one copy")
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
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
	if _, err := eng.Run(context.Background(), time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("dump: %v", err)
	}

	p := eng.drl.posture(WormResult{}, 0)
	if p.Copies != 1 {
		t.Fatalf("weakest-covered copies = %d, want 1", p.Copies)
	}
	for _, c := range p.Checks {
		if c.Name == "3 copies" && c.Status != PostureWarn {
			t.Errorf("single-copy '3 copies' = %v, want WARN", c.Status)
		}
	}
}

// placeArchive records one archive of a run on a single medium — a catalog-only
// copy (no bucket opened), the primitive the copy-diagnosis tests build runs from.
func placeArchive(t *testing.T, cat *catalog.Catalog, slug string, level int, at time.Time, medium string) {
	t.Helper()
	arch := record.Archive{Run: record.IDFromTime(at), DLE: slug, Level: level, CreatedAt: at}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: medium, Pos: 1}}, Commit: archiveio.FilePos{Label: medium, Pos: 2}}
	if err := cat.AddArchive(arch, medium, pos); err != nil {
		t.Fatal(err)
	}
}

// twoLandingEngine builds an engine whose one DLE fans out to two disk landings —
// the fan-out shape the copy-diagnosis tests reason over.
func twoLandingEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	src := t.TempDir()
	cfg := &config.Config{
		Landing: config.MediumList{"disk", "disk2"},
		Media: map[string]config.Media{
			"disk":  {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
			"disk2": {Type: "disk", Params: map[string]string{"path": t.TempDir()}},
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
	return eng, config.Slug("localhost", src)
}

// TestPostureTrippedLaneBackfillable is the user-reported case: a DLE fans out to
// two landings but one lane failed once, so a run holds only its first copy. The
// "3 copies" check must still WARN (a single-copy window is real), but attribute
// the gap to the missing medium and point at `nb sync` — not the bare "3-2-1
// wants 2 backups" that reads as a misconfiguration.
func TestPostureTrippedLaneBackfillable(t *testing.T) {
	eng, slug := twoLandingEngine(t)
	// The fan-out landed on disk; the disk2 lane tripped, so no disk2 copy exists.
	placeArchive(t, eng.cat, slug, 0, time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), "disk")

	p := eng.drl.posture(WormResult{}, 0)
	if p.Copies != 1 {
		t.Fatalf("weakest-covered copies = %d, want 1", p.Copies)
	}
	var got PostureCheck
	for _, c := range p.Checks {
		if c.Name == "3 copies" {
			got = c
		}
	}
	if got.Status != PostureWarn {
		t.Fatalf("'3 copies' = %v, want WARN", got.Status)
	}
	for _, want := range []string{"disk2", "nb sync", "backfill"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q missing %q — should name the tripped medium and the remedy", got.Detail, want)
		}
	}
	if strings.Contains(got.Detail, "add a second landing") {
		t.Errorf("detail %q offers the structural remedy, but this is a backfillable gap", got.Detail)
	}
}

// TestPostureTrippedLaneAmongSiblings is the exact reported case: a run dumps two
// DLEs to two landings, and one DLE's archive fails to land on disk2 while the
// other DLE reaches both media. The run still has a placement on disk2 (its
// sibling), so a run-level copy count reads two and stays silent — only per-archive
// reasoning sees that one archive holds a single copy, WARNs, and names it.
func TestPostureTrippedLaneAmongSiblings(t *testing.T) {
	eng, _ := twoLandingEngine(t)
	// A second DLE on the same host so the run carries two archives.
	eng.cfg.Sources = append(eng.cfg.Sources, config.DLE{Host: "localhost", Path: t.TempDir()})
	full, tripped := eng.cfg.DLEs()[0].Name(), eng.cfg.DLEs()[1].Name()
	at := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	// full lands on both media; tripped only reaches disk (its disk2 lane failed).
	placeArchive(t, eng.cat, full, 0, at, "disk")
	placeArchive(t, eng.cat, full, 0, at, "disk2")
	placeArchive(t, eng.cat, tripped, 0, at, "disk")

	p := eng.drl.posture(WormResult{}, 0)
	if p.Copies != 1 {
		t.Fatalf("weakest-covered copies = %d, want 1 (the tripped archive)", p.Copies)
	}
	var got PostureCheck
	for _, c := range p.Checks {
		if c.Name == "3 copies" {
			got = c
		}
	}
	if got.Status != PostureWarn {
		t.Fatalf("'3 copies' = %v (%q), want WARN — one archive holds a single copy", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, tripped) || !strings.Contains(got.Detail, "disk2") || !strings.Contains(got.Detail, "nb sync") {
		t.Errorf("detail %q should name the tripped DLE %q, its missing medium disk2, and `nb sync`", got.Detail, tripped)
	}
	if strings.Contains(got.Detail, full) {
		t.Errorf("detail %q should not implicate the fully-copied DLE %q", got.Detail, full)
	}
}

// TestPostureRotatedCopyOK guards the false-positive the other direction: an older
// run whose second copy has aged out of a medium's retention is retention working,
// not a defect. With the current run fully fanned out, "3 copies" must read OK
// even though the weakest run holds a single copy.
func TestPostureRotatedCopyOK(t *testing.T) {
	eng, slug := twoLandingEngine(t)
	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	// The old full has rotated down to a single copy on disk; the recent full still
	// carries both copies — so disk2's absence on the old run is retention, not a gap.
	placeArchive(t, eng.cat, slug, 0, old, "disk")
	placeArchive(t, eng.cat, slug, 0, recent, "disk")
	placeArchive(t, eng.cat, slug, 0, recent, "disk2")

	p := eng.drl.posture(WormResult{}, 0)
	if p.Copies != 1 {
		t.Fatalf("weakest-covered copies = %d, want 1 (the rotated old run)", p.Copies)
	}
	for _, c := range p.Checks {
		if c.Name == "3 copies" && c.Status != PostureOK {
			t.Errorf("'3 copies' = %v (%q), want OK — a rotated-out copy is retention, not a defect", c.Status, c.Detail)
		}
	}
}

// TestPostureKeyConfiguredNotReady exercises postureKey's "configured but not ready"
// branch: encryption is configured but the key reference cannot be resolved, so the key
// check warns (the lost-key mode checksum verification cannot see).
func TestPostureKeyConfiguredNotReady(t *testing.T) {
	cfg := &config.Config{
		Landing:  config.MediumList{"disk"},
		Media:    map[string]config.Media{"disk": {Type: "disk", Params: map[string]string{"path": t.TempDir()}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
		Encrypt:  config.EncryptConfig{Scheme: "gpg", PassphraseFile: filepath.Join(t.TempDir(), "no-such-passphrase")},
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	name, st, detail := eng.drl.postureKey()
	if name != "key reachable" || st != PostureWarn {
		t.Fatalf("postureKey = (%q, %v, %q), want a WARN", name, st, detail)
	}
	if !strings.Contains(detail, "not ready") {
		t.Errorf("detail = %q, want it to say the key is not ready", detail)
	}
}

// TestPostureCapacityOver exercises postureCapacity's over-capacity branch: a landing
// whose stored bytes exceed its capacity warns to run `nb prune`.
func TestPostureCapacityOver(t *testing.T) {
	cfg := &config.Config{
		Landing:  config.MediumList{"cloud"},
		Media:    map[string]config.Media{"cloud": {Type: "cloud", Capacity: "1000", Params: map[string]string{"url": "mem://"}}},
		Sources:  []config.DLE{{Host: "localhost", Path: t.TempDir()}},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	cfg.Compress.Scheme = "none"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if eng.Capacity() <= 0 {
		t.Fatalf("cloud medium should have a bounded capacity, got %d", eng.Capacity())
	}

	// Record an archive well past capacity (no bucket is opened — this is catalog-only).
	at := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	arch := record.Archive{Run: record.IDFromTime(at), DLE: "d", Level: 0, Compressed: 5000, CreatedAt: at}
	pos := archiveio.ArchivePos{Parts: []archiveio.FilePos{{Label: "cloud", Pos: 1}}, Commit: archiveio.FilePos{Label: "cloud", Pos: 2}}
	if err := eng.cat.AddArchive(arch, "cloud", pos); err != nil {
		t.Fatal(err)
	}

	name, st, detail := eng.drl.postureCapacity()
	if name != "capacity OK" || st != PostureWarn {
		t.Fatalf("postureCapacity = (%q, %v, %q), want a WARN", name, st, detail)
	}
	if !strings.Contains(detail, "over capacity") {
		t.Errorf("detail = %q, want it to warn over capacity", detail)
	}
}
