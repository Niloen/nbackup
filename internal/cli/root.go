package cli

import (
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
		Short:   "Immutable, slot-based backups inspired by Amanda",
		Long:    rootLong,
		Version: Version,
		// Subcommands return errors that main turns into a non-zero exit via
		// Fatalf; cobra should not also print them or dump usage on failure.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&a.cfgPath, "config", "c", DefaultConfigPath, "path to config file")
	// --catalog has no short flag: a `-C`/`-c` pair distinguished only by case is
	// an easy slip on two heavily-used globals.
	pf.StringVar(&a.catalog, "catalog", "", "catalog directory (overrides config)")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "suppress progress output")

	// Convention: inspect with a noun (`slot`, `medium`), act with a flat verb.
	root.AddCommand(
		newPlanCmd(a),
		newDumpCmd(a),
		newStatusCmd(a),
		newSlotCmd(a),
		newMediumCmd(a),
		newVerifyCmd(a),
		newRecoverCmd(a),
		newCopyCmd(a),
		newSyncCmd(a),
		newLabelCmd(a),
		newLoadCmd(a),
		newPruneCmd(a),
		newRebuildCmd(a),
	)
	return root
}

// Execute runs the nb command tree. Errors are returned for main to report.
func Execute() error {
	return NewRootCmd().Execute()
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

// logf returns the progress logger, or nil when --quiet is set.
func (a *app) logf() engine.Logf {
	if a.quiet {
		return nil
	}
	return logfStdout
}
