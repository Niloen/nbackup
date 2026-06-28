package xfer

import "github.com/Niloen/nbackup/internal/programs"

// split.go holds the one rule the encode (dump) and decode (restore) operations share for placing
// an archive's scheme transforms. Each compress/encrypt step runs either fused with the endpoint
// program — the client's tar when encoding, the target's tar when decoding, so plaintext never
// leaves the client and a remote restore ships compressed bytes — or in the local server filters
// (the pinned middle). Keeping the split here means the two directions cannot drift, and the "none"
// identity drops out in exactly one place.

// Transform is one scheme step: a resolved compress/encrypt command and where it runs (Fused with
// the endpoint's tar, or in the local server filters). A zero command (a compress or encrypt
// "none") is an identity and vanishes from the pipeline.
type Transform struct {
	Cmd   programs.Cmd
	Fused bool
}

// SplitTransforms partitions steps, in pipeline order, into the commands to fuse onto the endpoint
// program chain (for the caller to splice after the source tar, or before the restore tar) and the
// local server Filters. Identity steps drop out of both.
func SplitTransforms(steps ...Transform) (fused []programs.Cmd, filters Filters) {
	filters = NewFilters()
	for _, s := range steps {
		switch {
		case s.Cmd.Name == "":
			continue
		case s.Fused:
			fused = append(fused, s.Cmd)
		default:
			filters = filters.Add(s.Cmd)
		}
	}
	return
}
