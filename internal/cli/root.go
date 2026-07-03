package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/lock"
)

// Version is reported by `nb --version`. It defaults to a pre-release marker and
// is stamped by release builds via
//
//	-ldflags "-X github.com/Niloen/nbackup/internal/cli.Version=<tag>"
//
// (`make build` stamps `git describe`; GoReleaser stamps the release tag).
var Version = "0.1.0-dev"

const rootLong = `NBackup - immutable, run-based backups.

NBackup produces one immutable run per execution: a directory of self-describing
archive files you can copy, inspect, and restore with standard tools. Runs live
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
		Short:   "Immutable, run-based backups",
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

	// Convention: inspect with a noun (`run`, `medium`), act with a flat verb.
	root.AddCommand(
		newInitCmd(a),
		newPlanCmd(a),
		newCheckCmd(a),
		newDumpCmd(a),
		newStatusCmd(a),
		newRunCmd(a),
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

	// Cobra's default `completion` group has no Run, so an unknown shell
	// (`nb completion tcsh`) fell through to help and exited 0 — success, to a
	// script. Materialize the default command now (the later auto-init sees it and
	// backs off) and make a bogus shell a real error like any other bad subcommand,
	// while a bare `nb completion` keeps printing the shell list.
	root.InitDefaultCompletionCmd()
	if compCmd, _, err := root.Find([]string{"completion"}); err == nil && compCmd.Name() == "completion" {
		compCmd.Args = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown shell %q for \"nb completion\" — supported: bash, zsh, fish, powershell", args[0])
		}
		compCmd.RunE = func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		}
	}
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
			fmt.Printf("nb version %s\n", Version)
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
	// SIGINT/SIGTERM cancels the run's context (threaded down to the dump) instead of
	// killing the process outright — so a canceled `nb dump` unwinds, kills its in-flight
	// tar/compressor, and records a terminal status rather than leaving run-status.json
	// frozen at "running".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Notify on a channel rather than via signal.NotifyContext: that ctx is also canceled by
	// its stop func on normal exit, which would fire the goroutine below and print "canceling…"
	// even when nothing was canceled. A dedicated channel reacts only to a real signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	// On the first signal, cancel the run's context and tell the operator the cancel was heard —
	// unwinding a dump (killing the in-flight tar/compressor, draining the spool) is not instant,
	// so without this the terminal sits silent and looks hung. A second signal force-quits, in case
	// a graceful cancel itself hangs: stopping the notifier restores the default disposition so the
	// next signal terminates the process outright.
	//
	// The notice is held back a beat: it's about a *slow* unwind, so a command that exits promptly
	// on cancel (e.g. the read-only `nb status --watch` viewer) closes done first and stays quiet —
	// no dump-flavored "canceling…" where nothing was being canceled.
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			signal.Stop(sigCh)
			cancel()
			select {
			case <-time.After(150 * time.Millisecond):
				fmt.Fprintln(os.Stderr, "\ncanceling… (press Ctrl-C again to force quit)")
			case <-done:
			}
		case <-done:
		}
	}()

	err := NewRootCmd().ExecuteContext(ctx)
	close(done)
	if err != nil && strings.HasPrefix(err.Error(), "unknown command") {
		return fmt.Errorf("%w\nRun 'nb --help' for usage", err)
	}
	return err
}

// loadForWrite reads full configuration for commands whose runs may write media
// (dump/copy/sync/…) and their previews: the config file must exist and parse, and
// --catalog is refused when the config defines media (see loadConfig). It applies
// the global --config and --catalog flags.
func (a *app) loadForWrite() (*config.Config, error) {
	return loadConfig(a.cfgPath, a.catalog)
}

// loadOrDefaultCatalog reads configuration for read-only browsing commands
// (run/dle/medium/report/…): when no config file exists it synthesizes a default
// local catalog so a bare `nb run` can still browse.
func (a *app) loadOrDefaultCatalog() (*config.Config, error) {
	return loadConfigOrDefaultCatalog(a.cfgPath, a.catalog)
}

// loadRequired reads configuration for read-only assertion commands (verify,
// status), erroring when no config file exists instead of synthesizing a default
// catalog — a monitor must see a failure, not a green answer about nothing.
func (a *app) loadRequired() (*config.Config, error) {
	return loadConfigRequired(a.cfgPath, a.catalog)
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
	eng, err := engine.New(cfg)
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
	eng, err = engine.New(cfg)
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
