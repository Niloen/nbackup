package cli

import (
	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
)

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
		Use:   "nb",
		Short: "Immutable, slot-based backups inspired by Amanda",
		Long:  rootLong,
		// Subcommands return errors that main turns into a non-zero exit via
		// Fatalf; cobra should not also print them or dump usage on failure.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&a.cfgPath, "config", "c", DefaultConfigPath, "path to config file")
	pf.StringVarP(&a.catalog, "catalog", "C", "", "catalog directory (overrides config)")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "suppress progress output")

	root.AddCommand(
		newPlanCmd(a),
		newDumpCmd(a),
		newSlotCmd(a),
		newVerifyCmd(a),
		newRestoreCmd(a),
		newCopyCmd(a),
		newLabelCmd(a),
		newCatalogCmd(a),
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

// logf returns the progress logger, or nil when --quiet is set.
func (a *app) logf() engine.Logf {
	if a.quiet {
		return nil
	}
	return logfStdout
}
