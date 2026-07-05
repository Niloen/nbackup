package media

import "fmt"

// Range selects a byte sub-range [Off, Off+Len) of a payload — the read request's
// one vocabulary, from the Volume seam up through the archive readers. Len <= 0 means
// "to the payload's end", so the ZERO VALUE selects the whole payload: a plain read
// is the range read's special case, not a separate path (the same construction as
// record.ShapeStream == ""). The flip side is deliberate: there is no way to express
// an empty range — no reader wants zero bytes, and a forgotten Len must mean "all of
// it" (the pre-range behavior), never silently nothing.
type Range struct {
	Off int64
	Len int64 // <= 0 = to the payload's end
}

// IsWhole reports whether r selects the entire payload — the classic whole-file read.
// It is a real decision point, not just sugar: a whole read needs no size table to
// plan, works on any medium (ErrRangeUnsupported is only ever about sub-ranges), and
// is what arms a part's inline seal check (a sub-range cannot be checked against a
// whole-part seal).
func (r Range) IsWhole() bool { return r.Off == 0 && r.Len <= 0 }

// End returns the exclusive end offset, or -1 when the range runs to the payload's
// end — for interval arithmetic (coalescing, intersection) over possibly open ranges.
func (r Range) End() int64 {
	if r.Len <= 0 {
		return -1
	}
	return r.Off + r.Len
}

// Bound clamps r to a payload of total bytes, resolving an open end to a concrete
// length. It errors when Off lies outside the payload.
func (r Range) Bound(total int64) (Range, error) {
	if r.Off < 0 || r.Off > total {
		return Range{}, fmt.Errorf("range offset %d outside payload (%d bytes)", r.Off, total)
	}
	if r.Len <= 0 || r.Off+r.Len > total {
		return Range{Off: r.Off, Len: total - r.Off}, nil
	}
	return r, nil
}
