package drain

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
)

// funnel.go routes a landing write's VolumeSink control calls back to the Drainer's orchestrator,
// so the byte stream runs on whatever goroutine holds the landing (a copy goroutine, or a producer
// doing a direct write) while every volume roll's catalog write stays on the sole catalog writer.

// Funnel is the proxy VolumeSink the landing Writer is built over, paired with the channel its
// calls travel on. The engine wraps the landing's real sink with Funnel.Proxy() when it opens the
// writer, and hands the Funnel (and the captured real sink) to New; the Drainer serves the channel
// on its orchestrator goroutine.
type Funnel struct {
	proxy *proxySink
	reqCh chan sinkReq
}

// NewFunnel builds a Funnel whose Proxy the engine plugs into the landing Writer.
func NewFunnel() *Funnel {
	ch := make(chan sinkReq)
	return &Funnel{proxy: &proxySink{reqCh: ch}, reqCh: ch}
}

// Proxy is the VolumeSink to build the landing Writer over.
func (f *Funnel) Proxy() archiveio.VolumeSink { return f.proxy }

// proxySink is a VolumeSink whose NextPart/PlaceRecord touch neither the librarian nor the catalog:
// they send the call to the orchestrator over reqCh and block on the reply. The byte write the
// caller does on the returned volume is the data half, on the caller's goroutine; the control half
// (the sink call) runs on the orchestrator. The round-trip is synchronous, so the drive is never
// written from two goroutines.
type proxySink struct {
	reqCh chan<- sinkReq
}

func (p *proxySink) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{reply: reply}
	r := <-reply
	return r.vol, r.max, r.volume, r.epoch, r.err
}

func (p *proxySink) PlaceRecord(size int64) (media.Volume, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{placeRecord: true, size: size, reply: reply}
	r := <-reply
	return r.vol, r.volume, r.epoch, r.err
}

// sinkReq is a funnelled VolumeSink call: placeRecord selects PlaceRecord(size) over NextPart().
type sinkReq struct {
	placeRecord bool
	size        int64
	reply       chan sinkResp
}

// sinkResp is the orchestrator's reply: the union of NextPart's and PlaceRecord's returns (max is
// unused for PlaceRecord).
type sinkResp struct {
	vol    media.Volume
	max    int64
	volume string
	epoch  int
	err    error
}

// serve runs one funnelled sink call on the real WriteSink — on the orchestrator goroutine, so a
// roll's RecordVolume/recycle catalog writes land on the sole catalog writer.
func serve(real archiveio.VolumeSink, req sinkReq) sinkResp {
	if req.placeRecord {
		vol, volume, epoch, err := real.PlaceRecord(req.size)
		return sinkResp{vol: vol, volume: volume, epoch: epoch, err: err}
	}
	vol, max, volume, epoch, err := real.NextPart()
	return sinkResp{vol: vol, max: max, volume: volume, epoch: epoch, err: err}
}
