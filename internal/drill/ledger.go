package drill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// LedgerFile is the recoverability ledger stored in the catalog workdir. It is the
// drill layer's only persistent state — deliberately a single inspectable file
// (atomic temp+rename, no daemon), matching NBackup's "state lives in files" stance
// (the same shape as catalog.CacheFile and the run-status file).
const LedgerFile = "drill-ledger.json"

// Record is the last drill outcome for one DLE: when it was last drilled, how
// deeply, against which copy, for what point in time, and whether it passed (with
// the failure class when it did not). It is what lets a report frame recoverability
// as an SLO — "every DLE drilled within N days, 0 failures" — and flag the DLEs that
// are never-drilled or failing.
type Record struct {
	DLE       string    `json:"dle"`
	LastDrill time.Time `json:"last_drill"`       // when this DLE was last drilled (zero = never)
	Tier      string    `json:"tier"`             // drill.Tier token exercised
	Medium    string    `json:"medium"`           // source medium the drill read from
	AsOf      string    `json:"as_of"`            // point-in-time drilled (YYYY-MM-DD)
	SlotID    string    `json:"slot_id"`          // target slot the chain restored to
	OK        bool      `json:"ok"`               // passed
	Class     string    `json:"class,omitempty"`  // failure class token when !OK
	Detail    string    `json:"detail,omitempty"` // human-readable reason when !OK
}

// Ledger maps DLE name -> its last drill Record. It is loaded from and saved to the
// workdir; absent or unreadable history yields an empty ledger (a cold start where
// every DLE is "never drilled").
type Ledger struct {
	Records map[string]Record `json:"records"`
}

// Load reads the ledger from workdir, returning an empty ledger when the file does
// not exist yet (the cold-start case).
func Load(workdir string) (*Ledger, error) {
	l := &Ledger{Records: map[string]Record{}}
	data, err := os.ReadFile(filepath.Join(workdir, LedgerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("parse drill ledger: %w", err)
	}
	if l.Records == nil {
		l.Records = map[string]Record{}
	}
	return l, nil
}

// Save writes the ledger to workdir atomically (temp + rename), so a reader always
// sees a complete old-or-new file and an interrupted drill never leaves it torn.
func (l *Ledger) Save(workdir string) error {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(workdir, LedgerFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(workdir, LedgerFile))
}

// Update records (or replaces) a DLE's latest drill outcome.
func (l *Ledger) Update(r Record) {
	if l.Records == nil {
		l.Records = map[string]Record{}
	}
	l.Records[r.DLE] = r
}

// Get returns a DLE's record, if any.
func (l *Ledger) Get(dle string) (Record, bool) {
	r, ok := l.Records[dle]
	return r, ok
}

// Drilled reports whether a DLE has ever been drilled successfully recently enough:
// it has a record, that record passed, and its last drill is within window of now.
// A never-drilled or failing DLE is "not covered" and is what selection prioritizes
// and a report flags.
func (l *Ledger) Drilled(dle string, window time.Duration, now time.Time) bool {
	r, ok := l.Records[dle]
	if !ok || !r.OK || r.LastDrill.IsZero() {
		return false
	}
	return now.Sub(r.LastDrill) < window
}

// Coverage reports, for a set of configured DLEs, those that have never been
// drilled and how many are not covered within the window (never-drilled, or whose
// last drill is too old or failing — anything Drilled rejects). It is the pure
// coverage computation shared by the engine's drill audit and `nb report`, kept here
// in the leaf where the ledger lives so neither side reimplements it.
func (l *Ledger) Coverage(dles []string, window time.Duration, now time.Time) (never []string, overdue int) {
	for _, d := range dles {
		rec, ok := l.Records[d]
		if !ok || rec.LastDrill.IsZero() {
			never = append(never, d)
			overdue++
			continue
		}
		if !l.Drilled(d, window, now) {
			overdue++
		}
	}
	return never, overdue
}

// Sorted returns the records sorted by DLE name, for stable report rendering.
func (l *Ledger) Sorted() []Record {
	out := make([]Record, 0, len(l.Records))
	for _, r := range l.Records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DLE < out[j].DLE })
	return out
}
