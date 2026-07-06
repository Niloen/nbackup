package web

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// handleMetrics renders NBackup's point-in-time gauges in the Prometheus text
// exposition format (version 0.0.4), hand-written so `nb web` pulls in no client
// library and keeps no registry: every value is read from the same read-only Source
// the HTML pages use, once per scrape. It never 500s — an empty catalog yields a
// valid, mostly-empty exposition, not an error — so a scrape target is healthy from
// the first run. All names are lowercase and stable, timestamps are unix seconds.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	var b strings.Builder

	// Run history — the most recent run of each command.
	last := lastPerCommand(s.history(0))
	family(&b, "nbackup_last_run_success", "gauge",
		"Whether the most recent run of a command succeeded (1) or failed (0).")
	for _, run := range last {
		sample(&b, "nbackup_last_run_success", labels{"command": string(run.Command)}, boolValue(!run.Failed()))
	}
	family(&b, "nbackup_last_run_timestamp_seconds", "gauge",
		"Unix time the most recent run of a command finished.")
	for _, run := range last {
		if run.EndedAt.IsZero() {
			continue
		}
		sample(&b, "nbackup_last_run_timestamp_seconds", labels{"command": string(run.Command)}, float64(run.EndedAt.Unix()))
	}
	family(&b, "nbackup_last_run_duration_seconds", "gauge",
		"Wall-clock duration of the most recent run of a command.")
	for _, run := range last {
		sample(&b, "nbackup_last_run_duration_seconds", labels{"command": string(run.Command)}, durationSeconds(run.StartedAt, run.EndedAt))
	}

	// Per-DLE freshness — omit a DLE never backed up (no meaningful timestamp).
	family(&b, "nbackup_dle_last_backup_timestamp_seconds", "gauge",
		"Unix time each DLE's most recent archive (any level) committed; absent for a DLE never backed up.")
	for _, d := range s.src.DLESummaries() {
		if d.LastBackupAt.IsZero() {
			continue
		}
		sample(&b, "nbackup_dle_last_backup_timestamp_seconds", labels{"dle": d.DLE}, float64(d.LastBackupAt.Unix()))
	}

	family(&b, "nbackup_dle_count", "gauge", "Configured DLEs (backup sources).")
	sample(&b, "nbackup_dle_count", nil, float64(len(s.src.DLENames())))

	// Staleness — always emitted: the window is the dump cycle, which is always set.
	family(&b, "nbackup_dle_stale_count", "gauge",
		"Configured DLEs overdue against the dump cycle (never backed up, or older than one cycle).")
	sample(&b, "nbackup_dle_stale_count", nil, float64(len(s.src.StaleDLEs(now))))

	// Drill coverage — the same counts the home rollup shows.
	dh := s.drillHealth(now)
	family(&b, "nbackup_drill_overdue_count", "gauge",
		"Configured DLEs not covered by a passing drill within the drill window.")
	sample(&b, "nbackup_drill_overdue_count", nil, float64(dh.Overdue))
	family(&b, "nbackup_drill_failing_count", "gauge", "DLEs whose most recent drill failed.")
	sample(&b, "nbackup_drill_failing_count", nil, float64(len(dh.Failing)))

	// Media capacity.
	media := s.src.Media()
	family(&b, "nbackup_medium_used_bytes", "gauge", "Bytes currently stored on a medium.")
	for _, m := range media {
		sample(&b, "nbackup_medium_used_bytes", labels{"medium": m.Name}, float64(m.Used))
	}
	family(&b, "nbackup_medium_capacity_bytes", "gauge",
		"Configured capacity of a medium in bytes; absent for an unbounded medium.")
	for _, m := range media {
		if m.Capacity <= 0 {
			continue // unbounded — omit the series rather than emit a misleading 0
		}
		sample(&b, "nbackup_medium_capacity_bytes", labels{"medium": m.Name}, float64(m.Capacity))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// labels is a metric sample's label set. Keys are emitted in sorted order so a
// series' identity is byte-stable across scrapes.
type labels map[string]string

// family writes a metric family's HELP and TYPE header lines. The samples follow.
func family(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

// sample writes one series line: the metric name, its sorted label set, and value.
func sample(b *strings.Builder, name string, lb labels, v float64) {
	b.WriteString(name)
	if len(lb) > 0 {
		keys := make([]string, 0, len(lb))
		for k := range lb {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, `%s="%s"`, k, escapeLabelValue(lb[k]))
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'f', -1, 64)) // plain decimal: no exponent form for large unix seconds
	b.WriteByte('\n')
}

// escapeLabelValue escapes a label value per the Prometheus text format: a backslash,
// a double-quote, and a newline. Nothing else in a label value needs escaping.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// durationSeconds is a run's wall-clock duration, 0 when either bound is missing (so
// a partially-recorded run never yields an absurd epoch-relative span).
func durationSeconds(start, end time.Time) float64 {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start).Seconds()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
