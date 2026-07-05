package catalog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/fsx"
)

// This file is the catalog's usage ledger: the persisted history of each medium's
// stored bytes over time. It deliberately breaks the cache's rebuildability rule —
// a pruned or relabeled archive leaves no trace on the volume, so the *decline* it
// caused could never be reconstructed by a media scan; only a running record captures
// it. The ledger is therefore the catalog's second non-rebuildable exception (beside
// the DLEMeta force-full directive): precious bookkeeping, Amanda's curinfo stance of
// keeping historical stats in the catalog layer.
//
// Recording is not a sampler bolted onto commands: persist() — the one durability
// choke point every catalog mutation flows through — diffs each medium's stored bytes
// against its last recorded sample and appends a sample per medium that changed. So
// every byte-changing path (dump, copy, sync, prune, flush, relabel, rebuild) records
// by construction, and non-byte mutations (barcodes, force-full) record nothing.

// UsageFile is the append-only usage ledger in the catalog workdir: one compact JSON
// UsageSample per line (JSONL), newest last.
const UsageFile = "medium-usage.jsonl"

// maxUsageLines bounds the ledger's growth; past it the log is compacted by
// age-thinning (see thinUsage) via an atomic rewrite, the run-log.jsonl move.
const maxUsageLines = 5000

// usageThinAge is the age past which compaction thins samples to one per medium per
// day. Recent samples keep their full intra-run granularity; old ones only need to
// preserve the curve's daily shape (the series is a step function, so keeping each
// day's last sample preserves it).
const usageThinAge = 30 * 24 * time.Hour

// UsageSample is one medium's stored bytes at one instant — a point on its
// used-capacity-over-time curve. Capacity deliberately does not ride along: it is
// config/profile knowledge, not catalog knowledge, and the reader has it fresher.
type UsageSample struct {
	At     time.Time `json:"at"`
	Medium string    `json:"medium"`
	Used   int64     `json:"used"`
	Runs   int       `json:"runs"` // runs with a copy on the medium at this instant
}

// MediumUsage returns one medium's recorded usage curve, oldest first.
func (c *Catalog) MediumUsage(medium string) []UsageSample {
	var out []UsageSample
	for _, s := range c.usage {
		if s.Medium == medium {
			out = append(out, s)
		}
	}
	return out
}

// recordUsage appends a sample for each medium whose stored bytes changed since its
// last sample — called by persist after the cache write succeeds. It diffs over the
// union of currently-placed media and media the ledger already knows, so a medium
// emptied of its last archive still records its fall to zero. Best-effort by design:
// the ledger is bookkeeping, so a failed append must never fail the mutation whose
// archive already durably committed (the mindex-store stance), and the next byte
// change re-diffs against the last *recorded* sample, self-healing the gap.
func (c *Catalog) recordUsage() {
	totals := c.mediumTotals()
	names := make(map[string]bool, len(totals)+len(c.usageLast))
	for m := range totals {
		names[m] = true
	}
	for m := range c.usageLast {
		names[m] = true
	}
	at := time.Now().UTC()
	if c.now != nil {
		at = c.now().UTC()
	}
	var changed []UsageSample
	for m := range names {
		t := totals[m] // zero value for an emptied medium: 0 bytes, 0 runs
		if last, ok := c.usageLast[m]; ok && last == t.bytes {
			continue
		}
		changed = append(changed, UsageSample{At: at, Medium: m, Used: t.bytes, Runs: t.runs})
	}
	if len(changed) == 0 {
		return
	}
	if err := appendUsage(c.workdir, changed); err != nil {
		return // best-effort: the gap self-heals on the next change
	}
	for _, s := range changed {
		c.usageLast[s.Medium] = s.Used
	}
	c.usage = append(c.usage, changed...)
}

// mediumTotal is one medium's aggregate over the current entries.
type mediumTotal struct {
	bytes int64
	runs  int
}

// mediumTotals sums each placed medium's stored bytes and run count in one pass over
// the entries — the same archive-granular accounting MediumBytes/RunsOn do per medium,
// fused so the persist-time diff costs one walk regardless of how many media exist.
func (c *Catalog) mediumTotals() map[string]mediumTotal {
	totals := map[string]mediumTotal{}
	for _, e := range c.entries {
		for _, p := range e.Placements {
			t := totals[p.Medium]
			t.runs++
			for _, a := range e.Run.Archives {
				if p.Holds(a.DLE, a.Level) {
					t.bytes += a.Compressed
				}
			}
			totals[p.Medium] = t
		}
	}
	return totals
}

// loadUsage reads the ledger into the catalog at Open, tolerating a torn trailing
// line (a reader racing the appender) and treating any error as an empty history — an
// unreadable ledger must never block opening the catalog.
func (c *Catalog) loadUsage() {
	c.usageLast = map[string]int64{}
	samples, err := readUsage(filepath.Join(c.workdir, UsageFile))
	if err != nil {
		return
	}
	c.usage = samples
	for _, s := range samples {
		c.usageLast[s.Medium] = s.Used
	}
}

// appendUsage writes samples as compact JSONL lines to the ledger, creating the
// workdir if absent, then compacts if the file has outgrown its cap. A single small
// write is atomic on a local filesystem (the workdir is required to be local), so a
// concurrent reader sees whole lines.
func appendUsage(dir string, samples []UsageSample) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, s := range samples {
		line, err := json.Marshal(s)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	path := filepath.Join(dir, UsageFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return compactUsageIfLarge(path, samples[len(samples)-1].At)
}

// readUsage reads the ledger oldest-first. Only the final line may legitimately be
// torn (an in-flight append); an unparseable interior line is real corruption.
func readUsage(path string) ([]UsageSample, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), b...))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []UsageSample
	for i, b := range lines {
		var s UsageSample
		if err := json.Unmarshal(b, &s); err != nil {
			if i == len(lines)-1 {
				break
			}
			return nil, fmt.Errorf("parse %s line %d: %w", UsageFile, i+1, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// compactUsageIfLarge rewrites the ledger once it grows past its cap, atomically so a
// reader always sees a complete file. A cheap raw line count decides first, so the
// common under-cap append pays no JSON parse. Compaction is age-thinning, not a tail
// cut: a tail cut at holding-disk sample rates would silently shrink the growth
// baseline the projection reads, defeating the ledger's purpose.
func compactUsageIfLarge(path string, now time.Time) error {
	over, err := usageOverLineCap(path)
	if err != nil || !over {
		return err
	}
	all, err := readUsage(path)
	if err != nil {
		return err
	}
	thinned := thinUsage(all, now)
	if len(thinned) == len(all) {
		return nil // nothing old enough to thin; live with the size until there is
	}
	var buf bytes.Buffer
	for _, s := range thinned {
		line, err := json.Marshal(s)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return fsx.WriteFileAtomic(path, buf.Bytes(), 0o644)
}

// thinUsage keeps every sample younger than usageThinAge and, for older ones, the
// last sample per medium per UTC day — preserving the step curve's daily shape and
// each day's closing value while shedding intra-run detail no one zooms into a month
// later. Order is preserved (the input is append-ordered).
func thinUsage(all []UsageSample, now time.Time) []UsageSample {
	cutoff := now.Add(-usageThinAge)
	type key struct {
		medium string
		day    string
	}
	// Last index per (medium, day) among old samples: that index survives.
	lastOfDay := map[key]int{}
	for i, s := range all {
		if s.At.Before(cutoff) {
			lastOfDay[key{s.Medium, s.At.UTC().Format("2006-01-02")}] = i
		}
	}
	out := all[:0:0]
	for i, s := range all {
		if !s.At.Before(cutoff) || lastOfDay[key{s.Medium, s.At.UTC().Format("2006-01-02")}] == i {
			out = append(out, s)
		}
	}
	return out
}

// usageOverLineCap reports whether the ledger holds more than maxUsageLines non-blank
// lines, scanning raw lines without parsing JSON. A missing file is not over.
func usageOverLineCap(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	var n int
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if n++; n > maxUsageLines {
			return true, nil
		}
	}
	return false, sc.Err()
}
