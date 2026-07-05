// Package cli holds helpers shared by the nb* command-line tools. Commands are
// thin wrappers that build an engine.Engine from configuration and render its
// results.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
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

// today is today's date at local midnight — the run-date default and the
// past/future boundary the plan/dump guards compare against. Run dates are the
// operator's local calendar day (the day on the wall clock), so "today" and the
// guards reason in that same local zone.
func today() time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// ParseDate parses a YYYY-MM-DD date in the local zone, or returns today (local)
// when empty. Local so an explicit --date names the same calendar day the run id
// will carry.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return today(), nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, time.Local)
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
	if date.Before(today()) {
		return fmt.Errorf("cannot plan a run for %s: it is in the past, and planning only reflects history before the run date (so a past date reports a misleading cold start)", record.DateString(date))
	}
	return nil
}

// errPastDump rejects dumping for a date already behind today. Backdating a run
// is not a supported use case: the planner only consults history strictly before
// the run date, so a past date would mis-level the new run (climbing the global
// incremental level rather than starting that date's own chain) and splice an
// out-of-order archive into date-ordered restore chains. Today and future are fine.
func errPastDump(date time.Time) error {
	if date.Before(today()) {
		return fmt.Errorf("cannot dump for %s: it is in the past, and backdating a run is not supported (the planner builds on history before the run date) — use today's date", record.DateString(date))
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
	// --catalog synthesizes a throwaway disk landing for read-only commands to browse
	// a catalog directory with no config. On a command that writes (dump/copy/sync/…)
	// combining it with a config that already defines media would SILENTLY redirect the
	// backup into the catalog dir instead of the configured landing — a data-misroute
	// with no warning. Refuse it: --catalog is an inspection flag, and `workdir:` is how
	// you move the cache.
	if catalogOverride != "" && (len(cfg.Media) > 0 || len(cfg.Landing) > 0) {
		return nil, fmt.Errorf("--catalog cannot be combined with a config that defines media (it would redirect writes to %q instead of the configured landing) — drop --catalog, or set `workdir:` in the config to move the catalog cache", catalogOverride)
	}
	applyCatalog(cfg, catalogOverride)
	return cfg, nil
}

// loadConfigOrDefaultCatalog loads configuration for read-only commands. With --catalog it uses
// that directory directly (ignoring any config file). Otherwise it reads the config
// if present (surfacing parse/validation errors) or synthesizes a default
// catalog when no config file exists.
func loadConfigOrDefaultCatalog(cfgPath, catalogOverride string) (*config.Config, error) {
	if catalogOverride != "" {
		cfg := &config.Config{}
		applyCatalog(cfg, catalogOverride)
		return cfg, nil
	}
	if _, err := os.Stat(cfgPath); err != nil {
		// A -c path the operator gave explicitly but that does not exist is a hard
		// error: a typo'd path must not silently look like an empty catalog (exit 0)
		// to a script or monitor. Only the *default* nbackup.yaml being absent is
		// tolerated, so a bare `nb run` can still browse a local catalog.
		if cfgPath != DefaultConfigPath {
			return nil, fmt.Errorf("no config file at %s — check the -c path", cfgPath)
		}
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

// loadConfigRequired is loadConfigOrDefaultCatalog for read-only *assertion* commands (verify,
// status): it never synthesizes a default catalog when no config file exists. A
// monitor that scrapes the exit code must see a clear failure rather than a green
// "0 run(s) verified" in a directory that simply has no config. A --catalog override still
// lets it inspect an existing catalog directly.
func loadConfigRequired(cfgPath, catalogOverride string) (*config.Config, error) {
	if catalogOverride != "" {
		cfg := &config.Config{}
		applyCatalog(cfg, catalogOverride)
		return cfg, nil
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return nil, fmt.Errorf("no config file at %s — copy nbackup.example.yaml to %s and edit it, run nb init, pass -c <path>, or --catalog <dir> to inspect an existing catalog", cfgPath, cfgPath)
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
	if len(cfg.Media) == 0 && len(cfg.Landing) == 0 {
		setLocalLanding(cfg, "default", DefaultCatalog)
	}
}

func setLocalLanding(cfg *config.Config, name, path string) {
	cfg.Media = map[string]config.Media{
		name: {Type: "disk", Params: map[string]string{"path": path}},
	}
	cfg.Landing = config.MediumList{name}
	cfg.Workdir = path
}

// newTab returns a tabwriter with the package's uniform table geometry (two-space
// minimal cells, space-padded), so every `nb` listing aligns the same way.
func newTab(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

// noConfigHint renders the empty-listing message for a read-only command that fell
// back to the synthesized default catalog because no backup config was found —
// distinguishing "configured but nothing dumped yet" from "no config at all", so a
// newcomer isn't left with the false impression that an unconfigured listing
// succeeded meaningfully. `what` is the command's own empty-listing phrase.
// `catalogOverride` set means --catalog pointed at this (empty) directory directly,
// bypassing any config file entirely — the empty listing is about that directory,
// not a missing config, so the hint must not blame a config that may well exist.
func noConfigHint(what, catalogOverride string) string {
	if catalogOverride != "" {
		return fmt.Sprintf("%s (catalog directory %q has no runs yet)", what, catalogOverride)
	}
	return what + " (no backup config found — copy nbackup.example.yaml to nbackup.yaml and edit it, or pass -c <config>)"
}

// logfStdout writes progress lines to stdout.
func logfStdout(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// stdinReader is shared so successive prompts in one process keep any buffered
// input rather than dropping it between reads.
var stdinReader = bufio.NewReader(os.Stdin)

// attachOperator gives the engine an interactive tape-swap operator only when
// stdin is a real terminal. With a pipe or cron stdin (not a terminal) no operator
// is attached, so a run that needs a tape swap errors cleanly instead of blocking
// forever — the same auto-detected unattended behavior `nb drill` uses. (A bare
// /dev/null stdin already aborted at EOF; this also covers a pipe that never sends
// the expected reply.)
func attachOperator(eng *engine.Engine) {
	if stdinIsTerminal() {
		eng.SetOperator(stdinOperator{})
	}
}

// stdinOperator drives single-drive (manual) tape swaps interactively: it shows
// what the drive holds and the reels in the room, then asks which to load. On a
// non-interactive run stdin is at EOF, so it aborts and the engine falls back to
// an actionable error instead of blocking.
type stdinOperator struct{}

func (stdinOperator) Swap(r librarian.SwapRequest) (string, bool) {
	fmt.Printf("\nmedium %q needs a tape: %s\n", r.Medium, r.Reason)
	fmt.Printf("in drive: %s\n", reelDesc(r.Loaded, r.Medium))
	if r.Expect != "" {
		fmt.Printf("this run expects tape %q (the oldest reusable volume — load it to recycle, or a fresh tape)\n", r.Expect)
	}
	if len(r.Shelf) == 0 {
		fmt.Println("no reels in the room to load")
		return "", false
	}
	fmt.Println("reels in the room (not in the drive):")
	for _, b := range r.Shelf {
		fmt.Printf("  %-10s %s\n", b.ID, reelDesc(b, r.Medium))
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

// reelDesc renders a reel/drive status for the operator prompt, reusing the
// inventory label classifier so the two never diverge. It uses the pure
// classifier only: the catalog-derived refinements (reclaimable orphans,
// appendability) are inventory concerns that don't belong in a swap prompt.
func reelDesc(b media.VolumeStatus, medium string) string {
	if b.ID == "" && b.Label == "" {
		return "(empty)"
	}
	label, status := classifyVolume(b, medium)
	if status == "full" {
		return fmt.Sprintf("%s (full)", label)
	}
	return label
}
