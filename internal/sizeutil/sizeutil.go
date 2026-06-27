// Package sizeutil parses and formats human-friendly sizes and durations
// used throughout NBackup configuration (e.g. "20TB", "30d").
package sizeutil

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseBytes parses a human-readable size such as "20TB", "500GB", "1024",
// "10MiB". Decimal units (KB/MB/GB/TB/PB) are powers of 1000; binary units
// (KiB/MiB/GiB/TiB/PiB) are powers of 1024. A bare number is bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	// Split numeric prefix from unit suffix.
	i := 0
	for i < len(s) && (s[i] == '.' || s[i] == '-' || s[i] == '+' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	numPart := strings.TrimSpace(s[:i])
	unit := strings.TrimSpace(strings.ToLower(s[i:]))

	if numPart == "" {
		return 0, fmt.Errorf("invalid size %q: expected a number with an optional unit, e.g. 20TB, 500GB, or 1048576", s)
	}
	num, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: expected a number with an optional unit, e.g. 20TB, 500GB, or 1048576", s)
	}
	if num < 0 {
		// A negative size is almost always a fat-fingered minus; reject it rather than
		// let it flow through as a negative byte count that downstream reads as
		// "≤0 = unbounded", silently disabling capacity enforcement.
		return 0, fmt.Errorf("invalid size %q: must not be negative", s)
	}

	var mult float64 = 1
	switch unit {
	case "", "b":
		mult = 1
	case "kb", "k":
		mult = 1e3
	case "mb", "m":
		mult = 1e6
	case "gb", "g":
		mult = 1e9
	case "tb", "t":
		mult = 1e12
	case "pb", "p":
		mult = 1e15
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	case "pib":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("unknown size unit %q", unit)
	}
	return int64(num * mult), nil
}

// ParseRate parses a throughput such as "50MB/s" or "500KB/s" into bytes per
// second. The "/s" suffix is optional and purely for readability — the value is
// always bytes per second, reusing ParseBytes for the size, so it is consistent
// with volume_size/capacity (bytes). A bare "50MB" means
// 50 MB/s.
func ParseRate(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if l := strings.ToLower(t); strings.HasSuffix(l, "/s") {
		t = strings.TrimSpace(t[:len(t)-len("/s")])
	}
	n, err := ParseBytes(t)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q: expected a size per second, e.g. 50MB/s or 500KB/s", s)
	}
	return n, nil
}

// FormatBytes renders a byte count in a compact human-readable form.
func FormatBytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}

// ParseDuration parses durations including a day ("d") and week ("w") suffix,
// which the standard library does not support. Examples: "30d", "2w", "12h".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var d time.Duration
	switch {
	case strings.HasSuffix(s, "d"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d = time.Duration(n * 24 * float64(time.Hour))
	case strings.HasSuffix(s, "w"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "w"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d = time.Duration(n * 7 * 24 * float64(time.Hour))
	default:
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: expected a number with a unit, e.g. 7d, 2w, or 24h", s)
		}
	}
	if d < 0 {
		// As with sizes, a negative retention/cycle is a typo, not a valid value;
		// reject it rather than let it be silently coerced to a default downstream.
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	return d, nil
}

// FormatDuration renders a duration in the day vocabulary the config uses (a
// whole number of days as "Nd"), falling back to the standard library form for
// sub-day or non-whole-day durations. So a one-cycle retention floor prints as
// "7d", matching `cycle: 7d`, rather than "168h0m0s".
func FormatDuration(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.String()
}

// FormatDaysHours renders a coarse age as whole days when it is at least a day,
// else whole hours ("5d", "7h"). Unlike FormatDuration (config-facing "Nd" with a
// standard-library fallback), the sub-day fallback is hours, for human-readable
// "how long ago" framing.
func FormatDaysHours(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// FormatElapsed renders a wall-clock span (run elapsed time, an ETA) as compact
// h/m/s, dropping leading zero units: "5s", "3m07s", "1h02m09s". This is the
// live-progress vocabulary, distinct from FormatDuration's config-facing "Nd".
func FormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
