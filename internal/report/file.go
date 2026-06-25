package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LogFile is the append-only run history in the catalog workdir: one compact JSON
// record per line (JSONL), newest last. It is the source `nb report` summarizes.
const LogFile = "run-log.jsonl"

// SummaryFile holds only the most recent run, pretty-printed and rewritten
// atomically each time — the single-object file a monitoring system can scrape
// without parsing the whole log (the same role run-status.json plays for progress).
const SummaryFile = "run-summary.json"

// maxLogLines bounds the history file's growth. Daily cron of a few commands is a
// few hundred lines a year, so this caps it generously while keeping the file small
// enough to stay inspectable; on overflow the log is compacted to its last
// maxLogLines records via an atomic rewrite (the drill.Ledger move).
const maxLogLines = 1000

// Append records one finished run: it appends r as a single compact line to
// LogFile and rewrites SummaryFile with r alone. The workdir is created if absent
// (it is the catalog workdir, created lazily elsewhere). Any error is returned for
// the caller to log as a warning — recording a run must never fail the run itself
// (the progress.NewFileSink contract).
func Append(dir string, r Run) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	path := filepath.Join(dir, LogFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// A single Write of a small record is atomic on a local filesystem (the workdir
	// is required to be local), so a concurrent reader sees whole lines — and the
	// reader also tolerates a torn trailing line, belt and suspenders.
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := writeSummary(filepath.Join(dir, SummaryFile), r); err != nil {
		return err
	}
	return compactIfLarge(path)
}

// writeSummary writes the latest run as a pretty single-object file, atomically.
func writeSummary(path string, r Run) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o644)
}

// writeFileAtomic writes data to a sibling temp file and renames it over path,
// so a concurrent reader never observes a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads the run history, oldest-first. A torn trailing line (a reader racing
// an append) is skipped rather than failing the read — `nb report` must never error
// because it caught a half-written record. A missing file yields an empty history.
func Load(dir string) ([]Run, error) {
	f, err := os.Open(filepath.Join(dir, LogFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var runs []Run
	sc := bufio.NewScanner(f)
	// Records can carry member/DLE lists; allow a generous line size.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var lines [][]byte
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
	for i, b := range lines {
		var r Run
		if err := json.Unmarshal(b, &r); err != nil {
			// Only the final line may legitimately be torn (an in-flight append); an
			// unparseable interior line is real corruption worth reporting.
			if i == len(lines)-1 {
				break
			}
			return nil, fmt.Errorf("parse %s line %d: %w", LogFile, i+1, err)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// Last returns the most recent n runs (oldest-first within the window). n <= 0
// returns the whole history.
func Last(dir string, n int) ([]Run, error) {
	runs, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	return runs, nil
}

// compactIfLarge rewrites the log to its last maxLogLines records when it has grown
// past the cap, atomically (temp + rename) so a reader always sees a complete file.
// It re-reads via Load so a torn trailing line is dropped in the same pass.
func compactIfLarge(path string) error {
	dir := filepath.Dir(path)
	runs, err := Load(dir)
	if err != nil || len(runs) <= maxLogLines {
		return err
	}
	runs = runs[len(runs)-maxLogLines:]
	var buf bytes.Buffer
	for _, r := range runs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}
