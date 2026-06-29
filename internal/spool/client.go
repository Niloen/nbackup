package spool

import (
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/media"
)

// client.go is the client side of the orchestrator-as-server: a backing-medium write's VolumeSink
// control calls (volume rolls) are remote calls served on the Spool's orchestrator goroutine, so
// the byte stream runs on whatever goroutine holds the backing medium (a copy goroutine, or a
// producer doing a direct write) while every volume roll's catalog write stays on the sole catalog
// writer (the server).

// Client is the proxy VolumeSink the backing-medium Writer is built over, paired with the channel its
// calls travel on and the real sink they ultimately reach. The engine calls Wrap(real) when it opens
// the writer — handing over the real backing sink and getting back the proxy to build over — and
// passes the Client to New. The Spool serves the channel on its orchestrator goroutine, forwarding
// each call to real.
type Client struct {
	proxy *clientSink
	reqCh chan sinkReq
	real  archiveio.VolumeSink // the backing medium's real sink, captured by Wrap
}

// NewClient builds a Client whose proxy the engine plugs into the backing Writer via Wrap.
func NewClient() *Client {
	ch := make(chan sinkReq)
	return &Client{proxy: &clientSink{reqCh: ch}, reqCh: ch}
}

// Wrap captures the backing medium's real sink and returns the proxy VolumeSink to build the Writer
// over. The Spool later serves the routed calls onto real (see serve).
func (c *Client) Wrap(real archiveio.VolumeSink) archiveio.VolumeSink {
	c.real = real
	return c.proxy
}

// clientSink is a VolumeSink whose NextPart/PlaceRecord touch neither the librarian nor the catalog:
// they send the call to the orchestrator over reqCh and block on the reply — a synchronous remote
// call. The byte write the caller does on the returned volume is the data half, on the caller's
// goroutine; the control half (the sink call) runs on the orchestrator (the server). The round-trip
// is synchronous, so the drive is never written from two goroutines.
type clientSink struct {
	reqCh chan<- sinkReq
}

func (p *clientSink) NextPart() (media.Volume, int64, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{reply: reply}
	r := <-reply
	return r.vol, r.max, r.volume, r.epoch, r.err
}

func (p *clientSink) PlaceRecord(size int64) (media.Volume, string, int, error) {
	reply := make(chan sinkResp, 1)
	p.reqCh <- sinkReq{placeRecord: true, size: size, reply: reply}
	r := <-reply
	return r.vol, r.volume, r.epoch, r.err
}

// sinkReq is a routed VolumeSink call: placeRecord selects PlaceRecord(size) over NextPart().
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

// serve runs one routed sink call on the real VolumeSink — on the orchestrator goroutine (the
// server), so a roll's RecordVolume/recycle catalog writes land on the sole catalog writer.
func serve(real archiveio.VolumeSink, req sinkReq) sinkResp {
	if req.placeRecord {
		vol, volume, epoch, err := real.PlaceRecord(req.size)
		return sinkResp{vol: vol, volume: volume, epoch: epoch, err: err}
	}
	vol, max, volume, epoch, err := real.NextPart()
	return sinkResp{vol: vol, max: max, volume: volume, epoch: epoch, err: err}
}
