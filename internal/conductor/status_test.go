package conductor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/logf"
	"github.com/Niloen/nbackup/internal/planner"
	"github.com/Niloen/nbackup/internal/progress"
	"github.com/Niloen/nbackup/internal/scheduler"
)

// preludeDeps wires the minimal Deps for a Run that dies in the prelude — after
// the estimate phase has written the status file, before any dump tracker exists.
// The Plan closure writes an estimating snapshot through its sink exactly like the
// real sizing pass does, so the test reproduces the file state the failure leaves
// behind. preflightErr/makeRoomErr choose which prelude step fails.
func preludeDeps(t *testing.T, workdir string, preflightErr, makeRoomErr error) Deps {
	t.Helper()
	dle := planner.DLE{Scope: archiver.Scope{Source: "/data"}, Host: "h1", DumpType: "std"}
	return Deps{
		Cat:   newCatalog(t),
		Flush: func(time.Time, logf.Logf) (int, error) { return 0, nil },
		Plan: func(date time.Time, sink progress.Sink) (*planner.Plan, error) {
			rows := []progress.Plan{{Name: dle.ID(), Slug: dle.Name()}}
			tr := progress.NewTracker(progress.EstimateRunID, progress.PhaseEstimating, 1, rows, time.Now, sink)
			tr.FinishDLE(dle.ID(), 0, 100, 0, nil)
			tr.SetPhase(progress.PhaseDone) // keepEstimating holds the file at "estimating"
			return &planner.Plan{Date: date, Items: []planner.Item{{DLE: dle, Level: 0, EstBytes: 100}}}, nil
		},
		Preflight: scheduler.PreflightDeps{
			CheckCompress:     func() error { return nil },
			ProbeReachable:    func(string) error { return preflightErr },
			PreflightDumptype: func(string, string, bool, map[string]bool) error { return nil },
		},
		MakeRoom: func(string, int64, time.Time, logf.Logf) (int64, error) {
			return 0, makeRoomErr
		},
		Workers:     1,
		NewFileSink: func() progress.Sink { return progress.NewFileSink(workdir, nil) },
		LandingsFor: func(planner.Item) []string { return []string{"gdrive"} },
	}
}

// A prelude failure (preflight or make-room, after the estimate phase has written
// the status file) must stamp the file terminal — or `nb status` and the web UI
// keep showing the dead run as "estimating" forever.
func TestRunPreludeFailureStampsStatusFailed(t *testing.T) {
	cases := []struct {
		name                      string
		preflightErr, makeRoomErr error
		wantErr                   string
		wantState                 progress.State
	}{
		// A single-landing route: the refusal downs the DLE's whole route, so the run
		// is fatal (a multi-landing route instead skips the refusing landing; see
		// TestRouteFatal for the judgment).
		{"make-room refusal", nil, errors.New("capacity 400.00 GB cannot hold the incoming"), "no landing on its route is usable", progress.StatePending},
		// A down host is the failure ladder's UNIT class: its DLEs are marked FAILED
		// (not pending) — here it is the only host, so nothing is plannable and the
		// run is fatal, but the status file still tells the per-DLE truth.
		{"preflight failure", errors.New("host h1 unreachable"), nil, "host h1 unreachable", progress.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			c := New(preludeDeps(t, workdir, tc.preflightErr, tc.makeRoomErr))
			_, err := c.Run(context.Background(), day, discardLog)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v; want containing %q", err, tc.wantErr)
			}
			snap, lerr := progress.Load(workdir)
			if lerr != nil {
				t.Fatalf("Load status: %v", lerr)
			}
			if snap.Phase != progress.PhaseFailed {
				t.Errorf("status file phase = %q; want failed (a non-terminal file shows the dead run as still estimating)", snap.Phase)
			}
			if !strings.Contains(snap.Err, tc.wantErr) {
				t.Errorf("status file err = %q; want containing %q", snap.Err, tc.wantErr)
			}
			if snap.EndedAt.IsZero() {
				t.Errorf("status file has no EndedAt; terminal snapshots must stamp one")
			}
			if len(snap.DLEs) != 1 || snap.DLEs[0].State != tc.wantState {
				t.Errorf("status file DLEs = %+v; want the planned DLE %q (nothing was dumped)", snap.DLEs, tc.wantState)
			}
		})
	}
}

// routeFatal is the any-lane-suffices judgment shared by make-room and window-open:
// a downed landing is fatal only when it empties some DLE's whole route.
func TestRouteFatal(t *testing.T) {
	dle := planner.DLE{Scope: archiver.Scope{Source: "/data"}, Host: "h1", DumpType: "std"}
	items := []planner.Item{{DLE: dle}}
	down := errors.New("capacity cannot hold the incoming")
	cases := []struct {
		name   string
		route  []string
		failed map[string]error
		fatal  bool
	}{
		{"nothing failed", []string{"s3", "gdrive"}, nil, false},
		{"one of two lanes down", []string{"s3", "gdrive"}, map[string]error{"gdrive": down}, false},
		{"whole route down", []string{"s3", "gdrive"}, map[string]error{"s3": down, "gdrive": down}, true},
		{"single-landing route down", []string{"gdrive"}, map[string]error{"gdrive": down}, true},
		{"unrelated landing down", []string{"s3"}, map[string]error{"gdrive": down}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Deps{LandingsFor: func(planner.Item) []string { return tc.route }})
			err := c.routeFatal(items, tc.failed)
			if (err != nil) != tc.fatal {
				t.Errorf("routeFatal(route=%v, failed=%v) = %v; want fatal=%v", tc.route, tc.failed, err, tc.fatal)
			}
		})
	}
}
