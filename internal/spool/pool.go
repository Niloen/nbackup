// Package spool is the holding disks' drain: the consuming half of a dump. A dump's producer
// stages each committed archive onto a holding disk; the drain copies it to the authoritative
// backing and reclaims the disk. A Pool spreads the staged dumps across the disks and, sized to
// each disk's capacity, back-pressures the producer; a backing failure aborts it so the producer
// stops and the run fails — never dropping data.
package spool

import (
	"sync"

	"github.com/Niloen/nbackup/internal/clerk"
	"github.com/Niloen/nbackup/internal/media"
)

// Disk is one disk in the holding Pool: the slot Session the producer stages onto and the volume (for
// the drain to read back and reclaim), plus its capacity budget. used is the landed-not-yet-drained
// byte count, guarded by Pool.mu.
type Disk struct {
	Name     string
	Session  *clerk.Session
	HoldVol  media.Volume // == the disk writer's lib.Volume()
	Capacity int64        // bytes; 0 = unbounded (no back-pressure)
	used     int64
}

// Pool is the capacity back-pressure and disk allocator across one or more holding disks. The
// producer acquires a disk for its DLE (round-robin, skipping full or too-small disks), charges
// the archive's bytes when it commits, and the next acquire blocks while every eligible disk is
// over capacity; the drain releases the bytes once the archive has landed and been reclaimed,
// waking a blocked producer. A backing failure aborts the pool, waking blocked producers (which
// then stop) so the run fails rather than overfilling. With a single disk it is a plain byte gate.
type Pool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	disks   []Disk
	cursor  int // round-robin allocation hand
	aborted error
}

func NewPool(disks []Disk) *Pool {
	p := &Pool{disks: disks}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// fits reports whether disk d could ever hold an archive estimated at est bytes — an unbounded disk
// fits anything, a bounded one fits an estimate strictly under its capacity (so est >= capacity
// routes direct).
func (d *Disk) fits(est int64) bool { return d.Capacity == 0 || est < d.Capacity }

// hasRoom reports whether disk d is currently under its capacity budget (always true unbounded).
func (d *Disk) hasRoom() bool { return d.Capacity == 0 || d.used < d.Capacity }

// Acquire picks a holding disk for a DLE estimated at est bytes, blocking while every disk that
// could fit it is over capacity. It returns direct=true when no disk can ever fit est (the DLE is
// too big for the largest disk and there is no unbounded one) — the caller dumps it straight to the
// backing. Allocation is round-robin from the cursor, skipping disks that can't fit est or have no
// room right now, so successive dumps spread across spindles. It returns the abort error if the
// drain has failed. The estimate is an uncompressed upper bound, so direct routing is conservative.
func (p *Pool) Acquire(est int64) (idx int, direct bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	anyFits := false
	for i := range p.disks {
		if p.disks[i].fits(est) {
			anyFits = true
			break
		}
	}
	if !anyFits {
		return 0, true, nil
	}
	for {
		if p.aborted != nil {
			return 0, false, p.aborted
		}
		for n := 0; n < len(p.disks); n++ {
			i := p.cursor % len(p.disks)
			p.cursor = (p.cursor + 1) % len(p.disks)
			if p.disks[i].fits(est) && p.disks[i].hasRoom() {
				return i, false, nil
			}
		}
		p.cond.Wait()
	}
}

// Charge accounts n landed bytes against disk idx's budget (does not block). Charging before the
// archive is enqueued keeps the accounting correct: the drain's Release happens-after.
func (p *Pool) Charge(idx int, n int64) {
	p.mu.Lock()
	p.disks[idx].used += n
	p.mu.Unlock()
}

// Release returns n charged bytes to disk idx and wakes any blocked producers.
func (p *Pool) Release(idx int, n int64) {
	p.mu.Lock()
	p.disks[idx].used -= n
	if p.disks[idx].used < 0 {
		p.disks[idx].used = 0
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Abort wakes every blocked producer — the backing is unreachable, so the run must fail rather than
// wait for space that will never free.
func (p *Pool) Abort(err error) {
	p.mu.Lock()
	if p.aborted == nil {
		p.aborted = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *Pool) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aborted
}

// Name, HoldVol, and Session resolve a disk by index (these read immutable fields, no lock).
func (p *Pool) Name(idx int) string            { return p.disks[idx].Name }
func (p *Pool) HoldVol(idx int) media.Volume   { return p.disks[idx].HoldVol }
func (p *Pool) Session(idx int) *clerk.Session { return p.disks[idx].Session }
