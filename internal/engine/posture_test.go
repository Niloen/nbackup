package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		New: func(opts media.Options) (media.Volume, error) {
			v, err := media.OpenVolume("disk", opts)
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
		Landing:  "wormlock",
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
		Landing: "disk",
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

// TestPostureSingleCopyWarns exercises the 1-backup-copy degraded branch: a single dump
// leaves one placement, so "3 copies" warns that 3-2-1 wants a second backup.
func TestPostureSingleCopyWarns(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "one copy")
	cfg := &config.Config{
		Landing:  "disk",
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

// TestPostureKeyConfiguredNotReady exercises postureKey's "configured but not ready"
// branch: encryption is configured but the key reference cannot be resolved, so the key
// check warns (the lost-key mode checksum verification cannot see).
func TestPostureKeyConfiguredNotReady(t *testing.T) {
	cfg := &config.Config{
		Landing:  "disk",
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
		Landing:  "cloud",
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
	pos := record.ArchivePos{DLE: "d", Level: 0, Parts: []record.FilePos{{Label: "cloud", Pos: 1}}, Commit: record.FilePos{Label: "cloud", Pos: 2}}
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
