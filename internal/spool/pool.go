package spool

import (
	"sync"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/ratelimit"
)

// pool.go is the holding-disk side of the spool: Disk + Pool, the capacity/writer back-pressure
// and round-robin allocator the spool stages dumps through before their drains copy them to the
// landing. See Acquire for the routing rule (too big for every disk ⇒ direct to the landing).

// Disk is one disk in the holding Pool: the run Storage the producer stages onto (and the drain
// reads back + reclaims through), plus its capacity budget. used sums two reservations against the
// disk — each dump's in-flight estimate (Acquire→Close) and each committed archive's landed bytes
// (Commit→drain) — guarded by Pool.mu.
type Disk struct {
	Name     string
	Alloc    archiveio.PartAllocator // places staged parts on the disk's volume (the opened medium's allocator)
	Storage  archivefs.WriteStore    // a holding disk is staged to, then read back + reclaimed by the drain
	Capacity int64                   // bytes; 0 = unbounded (no back-pressure)
	Lim      *ratelimit.Limiter      // byte-rate cap for staging writes to this disk (nil = uncapped)
	Writers  int                     // max concurrent staging writes (the medium's `writers`; 0 = uncapped)
	used     int64
	writing  int // staging writes in flight (Acquire→ReleaseWriter), counted against Writers
}

// Pool is the capacity back-pressure and disk allocator across one or more holding disks. The
// producer acquires a disk for its DLE (round-robin, skipping full or too-small disks), reserving
// the DLE's estimate against that disk for its in-flight write; the next acquire blocks while every
// eligible disk is over capacity. The producer frees that reservation when it closes the sink; a
// committed archive's landed bytes are charged until the drain copies them off and reclaims them,
// waking a blocked producer. A landing failure aborts the pool, waking blocked producers (which
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

// hasRoomFor reports whether disk d can fit a reservation of est more bytes within its capacity
// budget (always true unbounded).
func (d *Disk) hasRoomFor(est int64) bool { return d.Capacity == 0 || d.used+est <= d.Capacity }

// hasWriterSlot reports whether disk d may take another concurrent staging write.
func (d *Disk) hasWriterSlot() bool { return d.Writers == 0 || d.writing < d.Writers }

// Acquire picks a holding disk for a DLE estimated at est bytes, blocking while every disk that
// could fit it is over capacity. It returns direct=true when no disk can ever fit est (the DLE is
// too big for the largest disk and there is no unbounded one) — the caller dumps it straight to the
// landing. Allocation is round-robin from the cursor, skipping disks that can't fit est or have no
// room right now, so successive dumps spread across spindles. On success it reserves est against the
// chosen disk's budget for the dump's in-flight write — freed when the producer closes the sink — so
// the many producers that acquire up front cannot collectively overfill a disk while writing. It
// returns the abort error if the drain has failed. The estimate is an uncompressed upper bound, so
// both direct routing and the reservation are conservative.
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
			if p.disks[i].fits(est) && p.disks[i].hasRoomFor(est) && p.disks[i].hasWriterSlot() {
				p.disks[i].used += est // reserve the in-flight write; the producer frees it on Close
				p.disks[i].writing++   // take a writer slot; the producer frees it on Close (ReleaseWriter)
				return i, false, nil
			}
		}
		p.cond.Wait()
	}
}

// Charge adds n landed bytes to disk idx's budget (does not block). Acquire reserves a dump's
// estimate for its in-flight write; on commit the archive's actual bytes occupy the disk until the
// drain copies them off, so charge those too — a later Acquire then back-pressures on the drain
// backlog, not just on the dumps still writing. The estimate reservation and these landed bytes
// briefly overlap (the producer frees the estimate on Close), which only over-reserves. Release
// frees the landed bytes once the archive has drained.
func (p *Pool) Charge(idx int, n int64) {
	p.mu.Lock()
	p.disks[idx].used += n
	p.mu.Unlock()
}

// ReleaseWriter returns disk idx's writer slot when the producer closes its staging sink, waking
// any producer blocked on the disk's `writers` cap. It pairs with Acquire, like the estimate
// reservation (which Release frees separately — a committed archive's bytes outlive the write).
func (p *Pool) ReleaseWriter(idx int) {
	p.mu.Lock()
	if p.disks[idx].writing > 0 {
		p.disks[idx].writing--
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Release returns n bytes (a reservation or landed bytes) to disk idx and wakes any blocked producers.
func (p *Pool) Release(idx int, n int64) {
	p.mu.Lock()
	p.disks[idx].used -= n
	if p.disks[idx].used < 0 {
		p.disks[idx].used = 0
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Abort wakes every blocked producer — the landing is unreachable, so the run must fail rather than
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

// Name, Storage and Lim resolve a disk by index (these read immutable fields, no lock).
func (p *Pool) Name(idx int) string                  { return p.disks[idx].Name }
func (p *Pool) Storage(idx int) archivefs.WriteStore { return p.disks[idx].Storage }

// Alloc resolves a disk's part allocator by index.
func (p *Pool) Alloc(idx int) archiveio.PartAllocator { return p.disks[idx].Alloc }
func (p *Pool) Lim(idx int) *ratelimit.Limiter        { return p.disks[idx].Lim }
