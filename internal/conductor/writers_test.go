package conductor

import (
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
)

// landingWriters resolves a landing's write concurrency: the medium's `writers` cap when set,
// else its natural width — drives (serial) or workers (concurrent) — with a serial medium never
// exceeding its drives.
func TestLandingWriters(t *testing.T) {
	pw := func(serial bool, allocs, writers int) PreparedWriter {
		return PreparedWriter{Allocs: make([]archiveio.PartAllocator, allocs), Serial: serial, Writers: writers}
	}
	cases := []struct {
		name    string
		pw      PreparedWriter
		workers int
		want    int
	}{
		{"concurrent medium defaults to the worker count", pw(false, 1, 0), 4, 4},
		{"serial single drive defaults to one writer", pw(true, 1, 0), 4, 1},
		{"serial library defaults to one writer per drive", pw(true, 2, 0), 4, 2},
		{"explicit writers caps a concurrent medium", pw(false, 1, 1), 4, 1},
		{"explicit writers may exceed workers (drains are not workers)", pw(false, 1, 3), 1, 3},
		{"serial library caps explicit writers at its drives", pw(true, 2, 4), 4, 2},
		{"serial library honors a lower explicit writers", pw(true, 2, 1), 4, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := landingWriters(c.pw, c.workers); got != c.want {
				t.Errorf("landingWriters(serial=%v stores=%d writers=%d, workers=%d) = %d; want %d",
					c.pw.Serial, len(c.pw.Allocs), c.pw.Writers, c.workers, got, c.want)
			}
		})
	}
}
