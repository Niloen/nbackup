package sizeutil

import (
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024,
		"20TB":  20e12,
		"500GB": 500e9,
		"10MiB": 10 << 20,
		"1k":    1000,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseBytes("12 furlongs"); err == nil {
		t.Errorf("expected error for bad unit")
	}
}

func TestParseRate(t *testing.T) {
	cases := map[string]int64{
		"50MB/s":  50e6,
		"500KB/s": 500e3,
		"1GB/s":   1e9,
		"50MB":    50e6, // bare size means per second
		"10MiB/s": 10 << 20,
	}
	for in, want := range cases {
		got, err := ParseRate(in)
		if err != nil {
			t.Fatalf("ParseRate(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseRate(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"fast/s", "-5MB/s", "/s"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q): expected error", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30d": 30 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
		"12h": 12 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}
