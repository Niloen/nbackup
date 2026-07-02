package spool

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// orchestrator.go is the routing seam of docs/design/concurrent-writes.md: the single coordinator
// goroutine every concurrent writer's control calls hop onto, and the WriteStore wrapper
// (routedWriteStore) that does the hopping. It knows nothing of lanes or holding disks — only
// which calls must be single-writer.

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

// volReq asks the orchestrator to run real's NextPart (or PlaceRecord, when place — size is the
// record's payload size, used by that mode only) and reply with the allocated volume; recordReq
// runs real's Record; reclaimReq runs store's Reclaim.
type volReq struct {
	real  archiveio.WriteStore
	place bool
	size  int64
	reply chan volResp
}

// volResp carries either mode's result: max is filled by NextPart only (PlaceRecord replies -1);
// the other fields are common to both.
type volResp struct {
	vol   media.Volume
	max   int64
	name  string
	epoch int
	err   error
}
type recordReq struct {
	real  archiveio.WriteStore
	res   archiveio.CommitResult
	reply chan error
}
type reclaimReq struct {
	store archiveio.Store
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
				resp.vol, resp.name, resp.epoch, resp.err = r.real.PlaceRecord(r.size)
			} else {
				resp.vol, resp.max, resp.name, resp.epoch, resp.err = r.real.NextPart()
			}
			r.reply <- resp
		case r := <-o.record:
			r.reply <- r.real.Record(r.res)
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
func (o *orchestrator) reclaimOn(store archiveio.Store, arch record.Archive, pos record.ArchivePos) error {
	reply := make(chan error, 1)
	o.reclaim <- reclaimReq{store: store, arch: arch, pos: pos, reply: reply}
	return <-reply
}

// routedWriteStore is a Session's WriteStore with its control calls hopped onto the orchestrator; the
// returned volume's AppendFile and byte writes stay on the caller's goroutine. Bounded is a constant,
// so it never crosses.
type routedWriteStore struct {
	real archiveio.WriteStore
	orch *orchestrator
}

func (r *routedWriteStore) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{real: r.real, reply: reply}
	x := <-reply
	return x.vol, x.max, x.name, x.epoch, x.err
}

func (r *routedWriteStore) PlaceRecord(size int64) (media.Volume, string, int, error) {
	reply := make(chan volResp, 1)
	r.orch.vol <- volReq{real: r.real, place: true, size: size, reply: reply}
	x := <-reply
	return x.vol, x.name, x.epoch, x.err
}

func (r *routedWriteStore) Bounded() bool { return r.real.Bounded() }

func (r *routedWriteStore) Record(res archiveio.CommitResult) error {
	reply := make(chan error, 1)
	r.orch.record <- recordReq{real: r.real, res: res, reply: reply}
	return <-reply
}
