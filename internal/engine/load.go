package engine

import (
	"math"
	"time"

	"github.com/Niloen/nbackup/internal/planner"
)

// DayLoad is one day's projected dump volume, split into the full portion (the
// lumpy mass promotion levels) and the incremental baseline. It is the per-day
// unit of the daily-load balance surfaced on `nb plan --days` and the /dles page.
type DayLoad struct {
	Date      time.Time
	FullBytes int64
	IncrBytes int64
	Fulls     int
	Incrs     int
	Promoted  int // fulls pulled forward by the leveler (a subset of Fulls)
}

// Total is the day's whole projected dump volume.
func (d DayLoad) Total() int64 { return d.FullBytes + d.IncrBytes }

// DailyLoad folds a simulated schedule into one DayLoad per day. The plans come
// from the caller's own Simulate/SimulateOffline pass (live or offline), so the
// load view and the schedule table it sits beside always agree — there is no
// second, independently-derived projection to drift.
func DailyLoad(plans []*planner.Plan) []DayLoad {
	out := make([]DayLoad, 0, len(plans))
	for _, p := range plans {
		d := DayLoad{Date: p.Date}
		for _, it := range p.Items {
			if it.Level == 0 {
				d.FullBytes += it.EstBytes
				d.Fulls++
				if it.Promoted {
					d.Promoted++
				}
			} else {
				d.IncrBytes += it.EstBytes
				d.Incrs++
			}
		}
		out = append(out, d)
	}
	return out
}

// LoadBalance summarizes how evenly a window's dump load falls across its days —
// the headline the /dles balance tiles and the `nb plan` footer read from. Peak is
// the heaviest day; CV (coefficient of variation, stddev/mean) is the spread the
// promoter's leveling works to shrink, so a lower CV means a flatter calendar.
type LoadBalance struct {
	Days     int
	Peak     DayLoad
	Mean     int64
	CV       float64
	Promoted int
}

// PeakRatio is the peak day's load as a multiple of the mean (1.0 == perfectly flat).
func (b LoadBalance) PeakRatio() float64 {
	if b.Mean <= 0 {
		return 0
	}
	return float64(b.Peak.Total()) / float64(b.Mean)
}

// Balance computes the spread statistics over the given days.
func Balance(loads []DayLoad) LoadBalance {
	b := LoadBalance{Days: len(loads)}
	if len(loads) == 0 {
		return b
	}
	var sum int64
	for _, d := range loads {
		sum += d.Total()
		b.Promoted += d.Promoted
		if d.Total() > b.Peak.Total() {
			b.Peak = d
		}
	}
	b.Mean = sum / int64(len(loads))
	if b.Mean > 0 {
		var ss float64
		for _, d := range loads {
			diff := float64(d.Total() - b.Mean)
			ss += diff * diff
		}
		b.CV = math.Sqrt(ss/float64(len(loads))) / float64(b.Mean)
	}
	return b
}
