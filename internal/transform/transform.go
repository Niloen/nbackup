// Package transform composes an archive's payload codec/cipher filters into a single
// reversible, host-placed pipeline. It owns one thing: the ORDER of the chain (compress,
// then encrypt) and the forward/reverse duality of running it. It does NOT run the
// pipeline, attribute faults, or know about volumes/parts — callers feed the stages it
// yields to hostexec.RunGrouped, fusing them with their own source (a dump) or consumer
// (an archiver extract) as each path requires. Placement is the engine's policy, handed
// in as data: every Stage carries the executor its filter runs on.
package transform

import "github.com/Niloen/nbackup/internal/hostexec"

// Stage is one placed filter: which reversible transform, and the host it runs on. It is
// the resolved form of a (scheme, options, executor) triple — the engine builds a Stage
// per filter from the record (scheme), config (options), and its own placement policy
// (executor), so the pipeline never sees those concerns separately.
type Stage struct {
	Filter hostexec.Filter
	Exec   hostexec.Executor
}

// Pipeline is an archive payload's reversible transform chain in ENCODE order (compress,
// then encrypt). Decode is the same chain reversed. An identity (none) filter in the
// chain contributes no stage in either direction.
type Pipeline []Stage

// Forward returns the encode-direction hostexec stages: each filter's Forward command in
// pipeline order, skipping identity (none) filters.
func (p Pipeline) Forward() []hostexec.Stage {
	out := make([]hostexec.Stage, 0, len(p))
	for _, s := range p {
		if s.Filter.Forward.Name == "" {
			continue
		}
		out = append(out, hostexec.Stage{Cmd: s.Filter.Forward, Exec: s.Exec})
	}
	return out
}

// Reverse returns the decode-direction hostexec stages: each filter's Reverse command in
// REVERSE pipeline order, skipping identity filters — a decode undoes the transforms in
// the opposite order they were applied (decrypt, then decompress).
func (p Pipeline) Reverse() []hostexec.Stage {
	out := make([]hostexec.Stage, 0, len(p))
	for i := len(p) - 1; i >= 0; i-- {
		s := p[i]
		if s.Filter.Reverse.Name == "" {
			continue
		}
		out = append(out, hostexec.Stage{Cmd: s.Filter.Reverse, Exec: s.Exec})
	}
	return out
}
