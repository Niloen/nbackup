package spool

import (
	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// orchestrator.go is the routing seam of docs/design/concurrent-writes.md: the single coordinator
// goroutine every concurrent writer's control calls hop onto, and the seam wrappers
// (routedAllocator, routedRecorder) that do the hopping. It knows nothing of lanes or holding
// disks — only which calls must be single-writer.

// orchestrator is the run's single-goroutine coordinator: it runs each routed control call — a
// librarian alloc/roll (NextPart/PlaceRecord), a catalog Record, or a holding drain's reclaim — to
// completion, serially, so those single-owner resources need no lock. It runs only these typed
// operations, never arbitrary work, and carries no bulk bytes.
type orchestrator struct {
	vol     chan volReq
	record  chan recordReq
	reclaim chan reclaimReq
	stop    chan struct{}
}

// volReq asks the orchestrator to run alloc's NextPart (or PlaceFile, when place — size is the
// file's payload size, used by that mode only) and reply with the allocated volume; recordReq
// runs rec's Record; reclaimReq runs store's Reclaim.
type volReq struct {
	alloc archiveio.PartAllocator
	place bool
	size  int64
	reply chan volResp
}

// volResp carries either mode's result: max is filled by NextPart only (PlaceFile replies -1);
// the other fields are common to both.
type volResp struct {
	vol   media.Volume
	max   int64
	name  string
	epoch int
	err   error
}
type recordReq struct {
	rec   archiveio.Recorder
	res   archiveio.CommitResult
	reply chan error
}
type reclaimReq struct {
	store archivefs.WriteStore
	arch  record.Archive
	pos   record.ArchivePos
	reply chan error
}

func newOrchestrator() *orchestrator {
	o := &orchestrator{
		vol:     make(chan volReq),
		record:  make(chan recordReq),
		reclaim: make(chan reclaimReq),
		stop:    make(chan struct{}),
	}
	go o.loop()
	return o
}

func (o *orchestrator) loop() {
	for {
		select {
		case r := <-o.vol:
			var resp volResp
			if r.place {
				resp.max = -1
				resp.vol, resp.name, resp.epoch, resp.err = r.alloc.PlaceFile(r.size)
			} else {
				resp.vol, resp.max, resp.name, resp.epoch, resp.err = r.alloc.NextPart()
			}
			r.reply <- resp
		case r := <-o.record:
			r.reply <- r.rec.Record(r.res)
		case r := <-o.reclaim:
			r.reply <- r.store.Reclaim(r.arch, r.pos)
		case <-o.stop:
			return
		}
	}
}

func (o *orchestrator) shutdown() { close(o.stop) }

// reclaimOn drops a staged archive from store on the orchestrator (Reclaim's catalog RemoveArchive is
// single-owner, like Record).
func (o *orchestrator) reclaimOn(store archivefs.WriteStore, arch record.Archive, pos record.ArchivePos) error {
	reply := make(chan error, 1)
	o.reclaim <- reclaimReq{store: store, arch: arch, pos: pos, reply: reply}
	return <-reply
}

// routedAllocator is a medium's PartAllocator with its allocation calls hopped onto the
// orchestrator; the returned volume's AppendFile and byte writes stay on the caller's goroutine.
// Bounded is a constant, so it never crosses.
type routedAllocator struct {
	real archiveio.PartAllocator
	orch *orchestrator
}

func (r *routedAllocator) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{alloc: r.real, reply: reply}
	x := <-reply
	return x.vol, x.max, x.name, x.epoch, x.err
}

func (r *routedAllocator) PlaceFile(size int64) (media.Volume, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{alloc: r.real, place: true, size: size, reply: reply}
	x := <-reply
	return x.vol, x.name, x.epoch, x.err
}

func (r *routedAllocator) Bounded() bool { return r.real.Bounded() }

// routedRecorder is a Session's Recorder with Record hopped onto the orchestrator — the sole
// catalog writer during a concurrent run.
type routedRecorder struct {
	real archiveio.Recorder
	orch *orchestrator
}

func (r *routedRecorder) Record(res archiveio.CommitResult) error {
	reply := make(chan error, 1)
	r.orch.record <- recordReq{rec: r.real, res: res, reply: reply}
	return <-reply
}
