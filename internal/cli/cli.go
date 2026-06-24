// Package cli holds helpers shared by the nb* command-line tools. Commands are
// thin wrappers that build an engine.Engine from configuration and render its
// results.
package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
)

// DefaultConfigPath is used when -c is not given.
const DefaultConfigPath = "nbackup.yaml"

// DefaultCatalog is used when neither --catalog nor config provides a catalog path.
const DefaultCatalog = "nbackup-catalog"

// Fatalf prints to stderr and exits non-zero.
func Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// ParseDate parses a YYYY-MM-DD date, or returns today (UTC) when empty.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC().Truncate(24 * time.Hour), nil
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: --date must be in YYYY-MM-DD format", s)
	}
	return d, nil
}

// errPastPlan rejects planning a run for a date already behind today. The planner
// only consults history strictly before the run date, so planning a past date
// ignores everything that happened since and reports a misleading from-scratch
// cold start — not a meaningful preview. Today and future dates are fine.
func errPastPlan(date time.Time) error {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if date.Before(today) {
		return fmt.Errorf("cannot plan a run for %s: it is in the past, and planning only reflects history before the run date (so a past date reports a misleading cold start)", date.Format("2006-01-02"))
	}
	return nil
}

// errPastDump rejects dumping for a date already behind today. Backdating a run
// is not a supported use case: the planner only consults history strictly before
// the run date, so a past date would mis-level the new slot (climbing the global
// incremental level rather than starting that date's own chain) and splice an
// out-of-order archive into date-ordered restore chains. Today and future are fine.
func errPastDump(date time.Time) error {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if date.Before(today) {
		return fmt.Errorf("cannot dump for %s: it is in the past, and backdating a run is not supported (the planner builds on history before the run date) — use today's date", date.Format("2006-01-02"))
	}
	return nil
}

// loadConfig loads configuration for commands that need full config (plan/dump),
// applying a catalog override and a default catalog path.
func loadConfig(cfgPath, catalogOverride string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	applyCatalog(cfg, catalogOverride)
	return cfg, nil
}

// loadConfigRO loads configuration for read-only commands. With --catalog it uses
// that directory directly (ignoring any config file). Otherwise it reads the config
// if present (surfacing parse/validation errors) or synthesizes a default
// catalog when no config file exists.
func loadConfigRO(cfgPath, catalogOverride string) (*config.Config, error) {
	if catalogOverride != "" {
		cfg := &config.Config{}
		applyCatalog(cfg, catalogOverride)
		return cfg, nil
	}
	if _, err := os.Stat(cfgPath); err != nil {
		cfg := &config.Config{}
		applyCatalog(cfg, "")
		return cfg, nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	applyCatalog(cfg, "")
	return cfg, nil
}

// applyCatalog applies a -C override (and a default when nothing is configured)
// by defining a disk landing medium pointing at the directory. The default is only
// synthesized for a truly bare config (no media AND no landing) — the no-config-file
// case read-only commands use to browse the local catalog. A config that names a
// `landing:` but omits its media is rejected by config.Validate instead, so its
// requested landing is never silently replaced by this default.
func applyCatalog(cfg *config.Config, catalogOverride string) {
	if catalogOverride != "" {
		setLocalLanding(cfg, "cli", catalogOverride)
		return
	}
	if len(cfg.Media) == 0 && cfg.Landing == "" {
		setLocalLanding(cfg, "default", DefaultCatalog)
	}
}

func setLocalLanding(cfg *config.Config, name, path string) {
	cfg.Media = map[string]config.Media{
		name: {Type: "disk", Params: map[string]string{"path": path}},
	}
	cfg.Landing = name
	cfg.Workdir = path
}

func newEngine(cfg *config.Config) (*engine.Engine, error) {
	return engine.New(cfg)
}

// logfStdout writes progress lines to stdout.
func logfStdout(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// stdinReader is shared so successive prompts in one process keep any buffered
// input rather than dropping it between reads.
var stdinReader = bufio.NewReader(os.Stdin)

// stdinOperator drives single-drive (manual) tape swaps interactively: it shows
// what the drive holds and the reels in the room, then asks which to load. On a
// non-interactive run stdin is at EOF, so it aborts and the engine falls back to
// an actionable error instead of blocking.
type stdinOperator struct{}

func (stdinOperator) Swap(r librarian.SwapRequest) (string, bool) {
	fmt.Printf("\nmedium %q needs a tape: %s\n", r.Medium, r.Reason)
	fmt.Printf("in drive: %s\n", reelDesc(r.Loaded))
	if r.Expect != "" {
		fmt.Printf("this run expects tape %q (the oldest reusable volume — load it to recycle, or a fresh tape)\n", r.Expect)
	}
	if len(r.Shelf) == 0 {
		fmt.Println("no reels in the room to load")
		return "", false
	}
	fmt.Println("reels in the room (not in the drive):")
	for _, b := range r.Shelf {
		fmt.Printf("  %-10s %s\n", b.ID, reelDesc(b))
	}
	def := suggestReel(r)
	prompt := "load which reel? (id or label"
	if def != "" {
		// With a default, a bare Enter takes it; aborting needs EOF (Ctrl-D). Without
		// one, an empty line aborts. Describe whichever applies — never both, since
		// "Enter = X" and "empty line aborts" contradict each other.
		prompt += fmt.Sprintf("; Enter = %s; Ctrl-D aborts", def)
	} else {
		prompt += "; empty line aborts"
	}
	fmt.Print(prompt + "): ")

	line, err := stdinReader.ReadString('\n')
	choice := strings.TrimSpace(line)
	if choice == "" {
		if err != nil || def == "" { // EOF (unattended) or no default to take
			fmt.Println()
			return "", false
		}
		choice = def
	}
	for _, b := range r.Shelf {
		if b.ID == choice || (b.Label != "" && b.Label == choice) {
			return b.ID, true
		}
	}
	fmt.Printf("no reel %q in the room\n", choice)
	return "", false
}

// suggestReel is the reel the engine would prefer: the one carrying the needed
// label (a read); for a write, the tape the run expects (the oldest reusable
// volume) if it is in the room, else the first blank reel.
func suggestReel(r librarian.SwapRequest) string {
	if r.Need != "" {
		for _, b := range r.Shelf {
			if b.Label == r.Need {
				return b.ID
			}
		}
		return ""
	}
	if r.Expect != "" {
		for _, b := range r.Shelf {
			if b.Label == r.Expect {
				return b.ID
			}
		}
	}
	for _, b := range r.Shelf {
		if b.Blank {
			return b.ID
		}
	}
	return ""
}

// reelDesc renders a reel/drive status for the operator prompt.
func reelDesc(b media.VolumeStatus) string {
	switch {
	case b.ID == "" && b.Label == "":
		return "(empty)"
	case b.Foreign:
		return "(foreign)"
	case b.Blank:
		return "(blank)"
	case b.Capacity > 0 && b.Used >= b.Capacity:
		return fmt.Sprintf("%s (full)", b.Label)
	default:
		return b.Label
	}
}
