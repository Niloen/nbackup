// Package report is NBackup's run-history and digest layer — the "did last night
// work, and is anything trending bad?" answer for an unwatched, cron-driven
// install. Every run-producing command (dump, sync, prune, verify, drill) records
// one machine-readable Run when it finishes; the records are appended to an
// inspectable history file in the catalog workdir (run-log.jsonl), with the latest
// also written as run-summary.json for a monitoring system to scrape. `nb report`
// renders the recent history as a digest, and the notify layer turns a record into
// an alert.
//
// It is a leaf, like drill/retention: pure record types plus their file persistence
// and rendering. It never imports the engine (the engine and the CLI import it),
// and — like progress.NewFileSink and drill.Ledger — a write error here is a
// warning the caller logs, never something that fails a backup.
package report

import "time"

// Command is the run-producing command that emitted a record.
type Command string

const (
	CommandDump   Command = "dump"
	CommandCopy   Command = "copy"
	CommandSync   Command = "sync"
	CommandPrune  Command = "prune"
	CommandVerify Command = "verify"
	CommandDrill  Command = "drill"
	CommandFlush  Command = "flush"
)

// Outcome is the coarse success/failure class the notify layer routes on.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// Run is one invocation's machine-readable summary — the uniform record across
// dump/sync/verify/drill/prune. It is a superset of fields; each command fills the
// ones it has and leaves the rest zero (so the JSON stays compact via omitempty).
// A slice of these is the history `nb report` summarizes and the basis of every
// notification.
type Run struct {
	Command   Command   `json:"command"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Outcome   Outcome   `json:"outcome"`
	// ExitClass is a short, stable token for *why* a run failed (e.g. "dump-failed",
	// "drill-failures", "verify-failures", "sync-error", "prune-error", "error"),
	// empty on success — the machine-routable analogue of drill.Class for a whole run.
	ExitClass string `json:"exit_class,omitempty"`
	// Error is the top-level error message when Outcome is OutcomeFailure.
	Error string `json:"error,omitempty"`

	// What ran / moved. "Bytes moved" is uniform across commands: bytes sealed
	// (dump), copied (sync), or freed (prune) by this run.
	RunID          string `json:"run_id,omitempty"`          // dump: the sealed run
	Archives       int    `json:"archives,omitempty"`        // dump: archive count
	BytesMoved     int64  `json:"bytes_moved,omitempty"`     // dump sealed / sync copied / prune freed
	RunsCopied     int    `json:"runs_copied,omitempty"`     // sync
	ArchivesPruned int    `json:"archives_pruned,omitempty"` // prune: archives reclaimed (prune's unit is the archive, not the run)
	Failures       int    `json:"failures,omitempty"`        // verify/drill failure count

	// Drill-only coverage + per-DLE health, so a digest can flag trends.
	Tier         string        `json:"tier,omitempty"` // drill: the tier exercised (what the drill tested)
	DrillHealth  []DrillHealth `json:"drill_health,omitempty"`
	Skipped      int           `json:"skipped,omitempty"`       // drill: unattended skips
	Overdue      int           `json:"overdue,omitempty"`       // drill: DLEs not covered within the window
	NeverDrilled []string      `json:"never_drilled,omitempty"` // drill: DLEs never drilled

	// DumpStats is the per-DLE breakdown of a dump:
	// level, original/output size, files, and dump time. Captured at seal time so the
	// dump report and its notification are historical, not just the last live run.
	DumpStats []DLEStat `json:"dump_stats,omitempty"`
}

// DLEStat is one DLE's statistics within a dump — the row of a dump
// report. Orig is the uncompressed archive stream, Out the compressed payload on the
// volume; Seconds is the dump duration (0 when timing was unavailable).
type DLEStat struct {
	DLE     string  `json:"dle"`            // internal slug (stable key)
	Host    string  `json:"host,omitempty"` // source host, for host:path display
	Path    string  `json:"path,omitempty"` // source path, for host:path display
	Level   int     `json:"level"`
	Orig    int64   `json:"orig"` // uncompressed bytes
	Out     int64   `json:"out"`  // compressed bytes on the volume
	Files   int     `json:"files"`
	Seconds float64 `json:"seconds,omitempty"` // dump duration; 0 = unknown
	// Promoted marks a full the planner pulled forward (not due today) and Reason
	// carries its explanation — so a report answers "why was tonight big" instead
	// of showing an unexplained level 0. Recorded from the run-status snapshot at
	// seal time; empty when the snapshot was unavailable (like Seconds).
	Promoted bool   `json:"promoted,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ID returns the host:path identity for display, falling back to the
// internal slug when host/path were not recorded.
func (d DLEStat) ID() string {
	if d.Host == "" && d.Path == "" {
		return d.DLE
	}
	return d.Host + ":" + d.Path
}

// DrillHealth is one DLE's outcome in a drill run alongside its prior ledger state,
// so a report can say *degrading* (passed before, failing now) versus a first-time
// failure, or simply confirm a still-healthy DLE.
type DrillHealth struct {
	DLE     string `json:"dle"`
	OK      bool   `json:"ok"`              // this run's outcome
	Class   string `json:"class,omitempty"` // drill.Class token when !OK
	WasOK   bool   `json:"was_ok"`          // the prior ledger record passed
	Drilled bool   `json:"drilled"`         // actually exercised this run (vs skipped)
	Bytes   int64  `json:"bytes,omitempty"` // egress this DLE's drill read off the medium
}

// Degrading reports a DLE that was passing and is now failing — the trend a digest
// must surface most loudly.
func (h DrillHealth) Degrading() bool { return h.Drilled && !h.OK && h.WasOK }

// Failed reports whether the run did not succeed.
func (r Run) Failed() bool { return r.Outcome == OutcomeFailure }
