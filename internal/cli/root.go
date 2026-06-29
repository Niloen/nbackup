package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/lock"
)

// Version is reported by `nb --version`. A single pre-release marker until the
// build stamps a real tag.
const Version = "0.1.0-dev"

const rootLong = `NBackup - immutable, slot-based backups.

NBackup produces one immutable slot per run: a directory of self-describing
archive files you can copy, inspect, and restore with standard tools. Slots live
on a Volume (local disk, a virtual tape, or S3) and stream between volumes with
"nb copy" (e.g. disk -> tape).

The global flags below work with every command and may appear anywhere on the
command line. Run "nb help <command>" for details on a specific command.`

// app holds the global flags shared by every subcommand. A single instance is
// bound to the root command's persistent flags and closed over by each
// subcommand's RunE.
type app struct {
	cfgPath string
	catalog string
	quiet   bool
}

// NewRootCmd builds the `nb` command tree with its persistent (global) flags.
func NewRootCmd() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:     "nb",
		Short:   "Immutable, slot-based backups",
		Long:    rootLong,
		Version: Version,
		// Subcommands return errors that main turns into a non-zero exit via
		// Fatalf; cobra should not also print them or dump usage on failure.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// A flag error names the command it occurred on, so the usage hint points at that
	// subcommand's help (`nb dump --help`) rather than the root — the help the operator
	// actually wants when a subcommand flag is wrong. Inherited by every subcommand.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w\nRun '%s --help' for usage", err, cmd.CommandPath())
	})

	// Replace cobra's default help command, whose unknown-topic path prints a message
	// but exits 0 (its handler is a Run, not a RunE) — a typo'd `nb help <topic>` must
	// fail like `nb <topic>` does so a script never mistakes it for success.
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, e := root.Find(args)
			if target == nil || e != nil {
				return fmt.Errorf("unknown help topic %q\nRun 'nb --help' for the command list", strings.Join(args, " "))
			}
			return target.Help()
		},
	})

	pf := root.PersistentFlags()
	pf.StringVarP(&a.cfgPath, "config", "c", DefaultConfigPath, "path to config file")
	// --catalog has no short flag: a `-C`/`-c` pair distinguished only by case is
	// an easy slip on two heavily-used globals.
	pf.StringVar(&a.catalog, "catalog", "", "catalog directory (overrides config)")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "suppress progress output")

	// Convention: inspect with a noun (`slot`, `medium`), act with a flat verb.
	root.AddCommand(
		newPlanCmd(a),
		newCheckCmd(a),
		newDumpCmd(a),
		newStatusCmd(a),
		newSlotCmd(a),
		newDleCmd(a),
		newMediumCmd(a),
		newVerifyCmd(a),
		newDrillCmd(a),
		newReportCmd(a),
		newRecoverCmd(a),
		newCopyCmd(a),
		newSyncCmd(a),
		newLabelCmd(a),
		newLoadCmd(a),
		newPruneCmd(a),
		newFlushCmd(a),
		newResetCmd(a),
		newRebuildCmd(a),
		newVersionCmd(),
	)
	return root
}

// newVersionCmd prints the version — a discoverable sibling of `nb --version`, since
// `nb version` is a natural thing to type.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the nb version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "nb version %s\n", Version)
			return nil
		},
	}
}

// Execute runs the nb command tree. Errors are returned for main to report. An
// unknown command gets a pointer to --help, since SilenceUsage suppresses cobra's
// own hint to keep operational failures terse. Unknown-flag errors are pointed at
// the right subcommand's help by SetFlagErrorFunc (in NewRootCmd), so they need no
// handling here.
func Execute() error {
	err := NewRootCmd().Execute()
	if err != nil && strings.HasPrefix(err.Error(), "unknown command") {
		return fmt.Errorf("%w\nRun 'nb --help' for usage", err)
	}
	return err
}

// load reads full configuration (for commands that may write), applying the
// global --config and --catalog flags.
func (a *app) load() (*config.Config, error) {
	return loadConfig(a.cfgPath, a.catalog)
}

// loadRO reads configuration for read-only commands.
func (a *app) loadRO() (*config.Config, error) {
	return loadConfigRO(a.cfgPath, a.catalog)
}

// loadRORequire reads configuration for read-only assertion commands (verify),
// erroring when no config file exists instead of synthesizing a default catalog.
func (a *app) loadRORequire() (*config.Config, error) {
	return loadConfigRORequire(a.cfgPath, a.catalog)
}

// lockedEngine takes the per-config exclusive lock, then builds the engine —
// for commands that mutate the catalog or media. The lock is acquired before
// construction so it also covers the catalog write that New may trigger when it
// populates a cold cache. The returned release func unlocks; callers defer it.
// On any failure the lock is released and a nil engine is returned.
func (a *app) lockedEngine(cfg *config.Config) (*engine.Engine, func(), error) {
	lk, err := lock.Acquire(cfg.WorkdirPath())
	if err != nil {
		return nil, nil, err
	}
	eng, err := newEngine(cfg)
	if err != nil {
		lk.Release()
		return nil, nil, err
	}
	return eng, func() { lk.Release() }, nil
}

// engineFor builds the engine for a command that is read-only on a dry run but
// mutating on a real run. When mutating it takes the exclusive lock (so the
// returned release unlocks); when not, it builds a plain engine and returns a
// no-op release. Callers defer release() and attach the operator themselves
// (which differs per command), keeping that decision at the call site.
func (a *app) engineFor(cfg *config.Config, mutating bool) (eng *engine.Engine, release func(), err error) {
	if mutating {
		return a.lockedEngine(cfg)
	}
	eng, err = newEngine(cfg)
	if err != nil {
		return nil, nil, err
	}
	return eng, func() {}, nil
}

// logf returns the progress logger, or nil when --quiet is set.
func (a *app) logf() engine.Logf {
	if a.quiet {
		return nil
	}
	return logfStdout
}
