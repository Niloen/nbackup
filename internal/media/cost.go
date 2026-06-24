package media

import (
	"fmt"
	"strconv"
)

// Cost is a medium's pricing — the dollar peer of Profile's bytes. A medium prices
// itself: each medium type registers a CostFactory that builds this from its options
// (a cloud bucket reads its URL scheme to pick provider rates), applying any
// per-medium overrides. The zero value is UNPRICED — a local disk or tape carries no
// recurring cloud bill, so its reports omit cost entirely.
//
// Pricing is a flat estimate by design: a storage rate, an egress rate, and a
// per-request rate. NBackup deliberately does NOT model storage-class lifecycle
// tiers (Glacier/Deep Archive transitions) — those are operator-configured
// bucket-side, and a guess at which tier bytes currently sit in is more likely wrong
// than useful. The provider invoice stays authoritative; these are list-price
// estimates for planning, computed offline with no billing API.
type Cost struct {
	Provider           string  // label for the rate table in use, e.g. "aws-s3"
	StoragePerGiBMonth float64 // recurring $/GiB-month
	EgressPerGiB       float64 // $/GiB transferred out (read off the medium)
	GetPer1000         float64 // $ per 1000 read (GET) requests
}

// bytesPerGiB is the storage/transfer billing unit (cloud list prices are per GiB).
const bytesPerGiB = 1 << 30

// Priced reports whether the medium carries any recurring or transfer cost. Local
// media (disk, tape) are unpriced, so callers suppress their cost output rather than
// print a misleading "$0.00".
func (c Cost) Priced() bool {
	return c.StoragePerGiBMonth > 0 || c.EgressPerGiB > 0 || c.GetPer1000 > 0
}

// MonthlyStorage is the recurring monthly cost of storing bytes.
func (c Cost) MonthlyStorage(bytes int64) float64 {
	return float64(bytes) / bytesPerGiB * c.StoragePerGiBMonth
}

// ReadCost is the one-time cost of reading bytes (held in `objects` files) off the
// medium: egress plus the GET requests — the charge a restore, recover, or offsite
// drill spends before it can hand back any data.
func (c Cost) ReadCost(bytes, objects int64) float64 {
	return float64(bytes)/bytesPerGiB*c.EgressPerGiB + float64(objects)/1000*c.GetPer1000
}

// CostFactory builds a medium type's Cost from its options (connection params plus
// any cost overrides).
type CostFactory func(Options) (Cost, error)

var costFactories = map[string]CostFactory{}

// RegisterCost registers a Cost implementation under a medium type name, exactly as
// RegisterProfile does for capacity.
func RegisterCost(typ string, f CostFactory) { costFactories[typ] = f }

// OpenCost builds the Cost for a medium type. A type with no registered factory is
// unpriced (the zero Cost), mirroring how an unregistered profile is unbounded.
func OpenCost(typ string, opts Options) (Cost, error) {
	f, ok := costFactories[typ]
	if !ok {
		return Cost{}, nil
	}
	return f(opts)
}

// ParseRate parses an optional non-negative float cost override; "" means "unset"
// (leave the base rate as is), so a medium factory can overlay only what config sets.
func ParseRate(s string) (rate float64, set bool, err error) {
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid rate %q: must be a number", s)
	}
	if v < 0 {
		return 0, false, fmt.Errorf("invalid rate %q: must not be negative", s)
	}
	return v, true, nil
}
