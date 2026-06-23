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
	"github.com/Niloen/nbackup/internal/media"
)

// DefaultConfigPath is used when -c is not given.
const DefaultConfigPath = "nbackup.yaml"

// DefaultCatalog is used when neither -C nor config provides a catalog path.
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
	return time.Parse("2006-01-02", s)
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

// loadConfigRO loads configuration for read-only commands. With -C it uses that
// directory directly (ignoring any config file). Otherwise it reads the config
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

// applyCatalog applies a -C override (and a default when no media is configured)
// by defining a disk landing medium pointing at the directory.
func applyCatalog(cfg *config.Config, catalogOverride string) {
	if catalogOverride != "" {
		setLocalLanding(cfg, "cli", catalogOverride)
		return
	}
	if len(cfg.Media) == 0 {
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

func (stdinOperator) Swap(r engine.SwapRequest) (string, bool) {
	fmt.Printf("\nmedium %q needs a tape: %s\n", r.Medium, r.Reason)
	fmt.Printf("in drive: %s\n", reelDesc(r.Loaded))
	if len(r.Shelf) == 0 {
		fmt.Println("no reels in the room to load")
		return "", false
	}
	fmt.Println("reels in the room (not in any bay):")
	for _, b := range r.Shelf {
		fmt.Printf("  %-10s %s\n", b.Bay, reelDesc(b))
	}
	def := suggestReel(r)
	prompt := "load which reel? (id or label"
	if def != "" {
		prompt += fmt.Sprintf("; Enter = %s", def)
	}
	fmt.Print(prompt + "; empty line aborts): ")

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
		if b.Bay == choice || (b.Label != "" && b.Label == choice) {
			return b.Bay, true
		}
	}
	fmt.Printf("no reel %q in the room\n", choice)
	return "", false
}

// suggestReel is the reel the engine would prefer: the one carrying the needed
// label (a read), else the first blank reel (a write needs a writable tape).
func suggestReel(r engine.SwapRequest) string {
	if r.Need != "" {
		for _, b := range r.Shelf {
			if b.Label == r.Need {
				return b.Bay
			}
		}
		return ""
	}
	for _, b := range r.Shelf {
		if b.Blank {
			return b.Bay
		}
	}
	return ""
}

// reelDesc renders a reel/drive status for the operator prompt.
func reelDesc(b media.BayStatus) string {
	switch {
	case b.Bay == "" && b.Label == "":
		return "(empty)"
	case b.Blank:
		return "(blank)"
	case b.Capacity > 0 && b.Used >= b.Capacity:
		return fmt.Sprintf("%s (full)", b.Label)
	default:
		return b.Label
	}
}
