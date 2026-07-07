package librarian

import (
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// handOp is the operator at a REAL single drive: the shelf software shows it is
// always empty (a real drive has no addressable slots), so on each Swap it
// physically inserts the next reel by mutating the drive's fakeReel and returns
// ok with NO reel id — exactly what the CLI's physical-swap prompt returns after
// the human presses Enter.
type handOp struct {
	reel  *fakeReel
	queue []record.Label
	swaps int
}

func (o *handOp) Swap(r SwapRequest) (string, bool) {
	if len(r.Shelf) != 0 {
		panic("a real drive must present an empty shelf")
	}
	if len(o.queue) == 0 {
		return "", false
	}
	o.reel.lbl, o.reel.labeled = o.queue[0], true
	o.queue = o.queue[1:]
	o.swaps++
	return "", true // an unnamed physical swap: the label read identifies the reel
}

// TestPhysicalSwapSpansReels: a spanning write on a real (no-slot) single drive
// must be able to roll across SEVERAL operator-inserted reels — each swap
// returns no reel id, so the librarian must track the accepted reels by their
// read labels, never by the empty id (which used to false-trip "volume \"\" was
// already used" on the second swap). Regression for the mhvtl road-test finding.
func TestPhysicalSwapSpansReels(t *testing.T) {
	now := time.Now()
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reel := &fakeReel{lbl: record.Label{Name: "T1", Pool: "pool", Epoch: 1, WrittenAt: now}, labeled: true, capacity: 1000}
	op := &handOp{reel: reel, queue: []record.Label{
		{Name: "T2", Pool: "pool", Epoch: 1, WrittenAt: now},
		{Name: "T3", Pool: "pool", Epoch: 1, WrittenAt: now},
	}}
	l := New(fakeChanger{fakeDrive{reel}}, "pool", cat, op, false, 0)

	if _, _, err := l.PrepareWrite(true, "", now, nil); err != nil {
		t.Fatal(err)
	}
	tried := map[string]bool{}
	// Two consecutive rolls, as a 3-reel spanning run performs them, sharing the
	// run-wide tried set.
	name, _, _, err := l.Advance(true, tried, "", now, nil)
	if err != nil {
		t.Fatalf("first physical swap: %v", err)
	}
	if name != "T2" {
		t.Fatalf("first roll accepted %q, want T2", name)
	}
	name, _, _, err = l.Advance(true, tried, "", now, nil)
	if err != nil {
		t.Fatalf("second physical swap must not trip the empty-id bookkeeping: %v", err)
	}
	if name != "T3" {
		t.Fatalf("second roll accepted %q, want T3", name)
	}
	if op.swaps != 2 {
		t.Fatalf("operator swapped %d time(s), want 2", op.swaps)
	}
}

// TestPhysicalSwapReadMount: a restore needing a different reel on a real drive
// prompts the operator (empty shelf, Need set) and re-reads whatever was
// inserted, mounting once the wanted label appears.
func TestPhysicalSwapReadMount(t *testing.T) {
	now := time.Now()
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"T1", "T2"} {
		if err := cat.RecordVolume(record.Label{Name: n, Pool: "pool", Epoch: 1, WrittenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	reel := &fakeReel{lbl: record.Label{Name: "T1", Pool: "pool", Epoch: 1, WrittenAt: now}, labeled: true, capacity: 1000}
	op := &handOp{reel: reel, queue: []record.Label{{Name: "T2", Pool: "pool", Epoch: 1, WrittenAt: now}}}
	l := New(fakeChanger{fakeDrive{reel}}, "pool", cat, op, false, 0)

	if err := l.MountForRead("T2", 1); err != nil {
		t.Fatalf("mount via physical swap: %v", err)
	}
	if st, ok := l.loaded(); !ok || st.Label != "T2" {
		t.Fatalf("loaded = %+v, want T2", st)
	}
	if op.swaps != 1 {
		t.Fatalf("operator swapped %d time(s), want 1", op.swaps)
	}
}

// TestLoadHintMatchesShape: operator guidance must be runnable on the medium's
// actual shape — `nb load <slot>` is a robot/sim instruction a real single-drive
// operator cannot execute (found testing by hand: an empty real drive told the
// operator to run `nb load`).
func TestLoadHintMatchesShape(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A REAL drive: manual, no addressable slots → physical-insert wording.
	real := New(fakeChanger{fakeDrive{&fakeReel{}}}, "pool", cat, nil, false, 0)
	if got := real.loadHint(""); got != "insert a tape into the drive" {
		t.Fatalf("real drive hint = %q", got)
	}
	if got := real.loadHint("T1"); got != `insert the tape labeled "T1" into the drive` {
		t.Fatalf("real drive named hint = %q", got)
	}
	// The file-backed manual SIM has addressable slots — `nb load` IS its hands.
	v, err := media.OpenVolume("tape", media.Options{"dir": t.TempDir(), "manual": "true", "slots": "2", "volume_size": "1048576"}, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := New(v, "pool", cat, nil, false, 0)
	if got := sim.loadHint(""); got != "load one with `nb load pool <slot>`" {
		t.Fatalf("sim hint = %q", got)
	}
}
