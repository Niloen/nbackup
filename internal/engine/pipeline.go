package engine

import (
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/xfer"
)

// pipeline.go holds the one rule the encode (dump) and decode (restore) operations share for
// placing an archive's codec transforms. Each compress/encrypt step runs either fused with the
// endpoint program — the client's tar when encoding, the target's tar when decoding, so
// plaintext never leaves the client and a remote restore ships compressed bytes — or in the
// local server filters (xfer's pinned middle). Keeping the split here means the two directions
// cannot drift, and the "none" identity drops out in exactly one place.

// transform is one codec step: a resolved compress/encrypt command and where it runs (fused
// with the endpoint's tar, or in the local server filters). A zero command (codec/scheme
// "none") is an identity and vanishes from the pipeline.
type transform struct {
	cmd   programs.Cmd
	fused bool
}

// splitTransforms partitions steps, in pipeline order, into the commands to fuse onto the
// endpoint program chain (for the caller to splice after the source tar, or before the restore
// tar) and the local server Filters. Identity steps drop out of both.
func splitTransforms(steps ...transform) (fused []programs.Cmd, filters xfer.Filters) {
	filters = xfer.NewFilters()
	for _, s := range steps {
		switch {
		case s.cmd.Name == "":
			continue
		case s.fused:
			fused = append(fused, s.cmd)
		default:
			filters = filters.Add(s.cmd)
		}
	}
	return
}
