package cli

import (
	"fmt"
	"os"
	"path"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// newRecoverCmd implements `nb recover`: browse a DLE's files as of a date and
// recover a selection. With no selection flags it opens an
// interactive shell (setdate/cd/ls/add/extract); with --path it runs one-shot,
// and with --list it just prints a listing.
func newRecoverCmd(a *app) *cobra.Command {
	var dleName, dateStr, timeStr, dest, to, from string
	var paths []string
	var listOnly, all, force, yes bool
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Browse a date and recover selected files, or restore a whole DLE",
		Long: "Recover from backups as they stood on a date. Three modes:\n\n" +
			"  • interactive (no flags): a shell to browse and pick files — setdate, setdisk," +
			" disks, cd, ls, add, delete, list, extract.\n" +
			"  • file-level (--path / --list): recover the named files/dirs in one shot, or just" +
			" print a listing. Selected-file recovery never deletes.\n" +
			"  • whole-DLE (--all): rebuild an entire DLE (or every DLE) as of the date into --dest," +
			" replaying the full plus later incrementals so deletions are applied. This prunes the" +
			" destination to match the backup, so --dest must be empty unless --force.\n\n" +
			"Paths are relative to the DLE root. A bare --date resolves to the most recent run on" +
			" or before it — the same run the browse view and --all restore both use.",
		Example: "  nb recover\n" +
			"  nb recover --dle app01:/home --date 2026-06-20 --list --path /etc\n" +
			"  nb recover --dle app01:/home --date 2026-06-20 --path /etc/hosts --dest /tmp/out\n" +
			"  nb recover --dle app01:/home --date 2026-06-20 --all --dest /tmp/out",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadRO()
			if err != nil {
				return err
			}
			// Recovery reads media (extraction always; browsing too, when a member
			// index misses the cache and falls back to the on-medium copy), so it
			// takes the config lock like any medium-accessing command. An interactive
			// session holds it for the session's duration — a recovery in progress
			// outranks a cron dump.
			eng, unlock, err := a.lockedEngine(cfg)
			if err != nil {
				return err
			}
			defer unlock()
			attachOperator(eng)
			if all {
				if listOnly || len(paths) > 0 {
					return fmt.Errorf("--all restores the whole DLE and cannot be combined with --path/--list")
				}
				return runRecoverRestore(eng, dleName, dateStr, timeStr, dest, to, from, force, yes, a.logf())
			}
			if listOnly || len(paths) > 0 {
				return runRecoverBatch(eng, dleName, dateStr, timeStr, paths, dest, listOnly, yes, a.logf())
			}
			return runRecoverShell(eng, dleName, dateStr, timeStr, dest)
		},
	}
	cmd.Flags().StringVar(&dleName, "dle", "", "DLE to recover from (with --all, omit to restore every DLE)")
	cmd.Flags().StringVar(&dateStr, "date", "", "as-of date YYYY-MM-DD (default today); resolves to the most recent run on or before that day")
	cmd.Flags().StringVar(&timeStr, "time", "", "as-of point-in-time 'YYYY-MM-DD HH[:MM[:SS]]' (UTC); resolves to the most recent run committed in or before that period — reaches an earlier same-day run. Mutually exclusive with --date")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "file/dir to recover (repeatable); non-interactive")
	cmd.Flags().StringVar(&dest, "dest", "", "destination directory for recovered files")
	cmd.Flags().BoolVar(&listOnly, "list", false, "print a listing of --path (or the root) and exit")
	cmd.Flags().BoolVar(&all, "all", false, "restore the whole DLE (deletion-accurate) as of the date into --dest")
	cmd.Flags().BoolVar(&force, "force", false, "with --all, restore into a non-empty --dest (its contents are pruned to match the backup)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the egress-cost confirmation when reading from a cloud/cold medium")
	cmd.Flags().StringVar(&to, "to", "", "with --all, restore onto a remote client: host:path (host must be in the config's hosts:); tar runs on the client, and for an encrypt.at: client DLE so does decryption — the key stays on the client")
	cmd.Flags().StringVar(&from, "from", "", "with --all, read from this medium's copy specifically (e.g. the offsite tape) instead of auto-selecting any available copy")
	return cmd
}

// runRecoverRestore performs a whole-DLE, deletion-accurate restore as of a date —
// the folded-in `nb restore`. With --dle it restores that DLE; without, every DLE
// in the catalog, each into its own subdirectory of dest.
func runRecoverRestore(eng *engine.Engine, dleName, dateStr, timeStr, dest, to, from string, force, yes bool, logf engine.Logf) error {
	asOf, err := recoverAsOf(dateStr, timeStr)
	if err != nil {
		return err
	}
	// --to host:path restores onto a remote client (tar runs there) instead of a local
	// --dest. The two are mutually exclusive: --to carries its own destination path.
	var toHost, toPath string
	if to != "" {
		if dest != "" {
			return fmt.Errorf("--to and --dest are mutually exclusive (--to host:path carries the destination)")
		}
		h, p, ok := strings.Cut(to, ":")
		if !ok || h == "" || p == "" {
			return fmt.Errorf("--to must be host:path (e.g. app01:/restore)")
		}
		toHost, toPath = h, p
		// localhost is this machine, not a remote client, so `--to localhost:/path` is
		// just a local restore to that path — route it through --dest (same empty-dest
		// guard and --force) rather than demanding localhost be under hosts:.
		if toHost == "localhost" {
			dest, toHost, toPath = toPath, "", ""
		}
	} else if dest == "" {
		return fmt.Errorf("--dest (or --to host:path) is required for --all (whole-DLE restore)")
	}
	var dles []string
	specified := dleName != ""
	if specified {
		slug, ok := eng.ResolveDLE(dleName)
		if !ok {
			return fmt.Errorf("unknown DLE %q; known: %s", dleName, strings.Join(eng.DLEDisplay(), ", "))
		}
		dles = []string{slug}
	} else {
		dles = eng.DLENames()
		if len(dles) == 0 {
			return fmt.Errorf("no DLEs in the catalog")
		}
	}
	if !confirmRead(eng.RestoreCost(dles, asOf), yes) {
		return nil
	}
	for _, name := range dles {
		base := dest
		if toHost != "" {
			base = toPath
		}
		out := base
		// When --dle is omitted we restore every DLE, each into its own
		// subdirectory of dest — unconditionally, so a script reading dest/<dle>/…
		// behaves the same whether the catalog holds one DLE or many.
		if !specified {
			out = path.Join(base, name)
		}
		dst := out
		if toHost != "" {
			dst = toHost + ":" + out
		}
		fmt.Printf("restoring DLE %s as of %s -> %s\n", eng.DisplayDLE(name), asOf, dst)
		restoreOne := func() error {
			if toHost != "" {
				return eng.RestoreAsOfTo(name, asOf, toHost, out, from, logf)
			}
			return eng.RestoreAsOf(name, asOf, out, from, force, logf)
		}
		if err := restoreOne(); err != nil {
			// When restoring every DLE, one that has no backup yet as of the date is
			// expected; note it and continue rather than aborting the whole restore.
			if len(dles) > 1 {
				fmt.Printf("  skipped %s: %v\n", name, err)
				continue
			}
			return err
		}
	}
	fmt.Println("restore complete")
	return nil
}

// runRecoverBatch handles the non-interactive paths: --list prints a listing,
// otherwise --path selections are extracted into --dest.
func runRecoverBatch(eng *engine.Engine, dleName, dateStr, timeStr string, paths []string, dest string, listOnly, yes bool, logf engine.Logf) error {
	asOf, err := recoverAsOf(dateStr, timeStr)
	if err != nil {
		return err
	}
	if dleName == "" {
		return fmt.Errorf("--dle is required (known: %s)", strings.Join(eng.DLEDisplay(), ", "))
	}
	slug, ok := eng.ResolveDLE(dleName)
	if !ok {
		return fmt.Errorf("unknown DLE %q; known: %s", dleName, strings.Join(eng.DLEDisplay(), ", "))
	}
	tree, err := eng.OpenRecover(slug, asOf)
	if err != nil {
		return err
	}

	if listOnly {
		target := "/"
		if len(paths) > 0 {
			target = paths[0]
		}
		n, ok := tree.Lookup(target)
		if !ok {
			return fmt.Errorf("not found: %s%s", target, pathRootHint(target))
		}
		fmt.Printf("# %s as of %s (%s)\n", eng.DisplayDLE(slug), tree.AsOf, tree.TargetRun)
		printListing(n)
		if tree.HasIncrementals() {
			fmt.Println(fileLevelDeletionNote)
		}
		return nil
	}

	if dest == "" {
		return fmt.Errorf("--dest is required to recover files")
	}
	steps, err := tree.Collect(paths)
	if err != nil {
		if strings.HasPrefix(err.Error(), "not found: ") {
			return fmt.Errorf("%w%s", err, pathRootHint(strings.TrimPrefix(err.Error(), "not found: ")))
		}
		return err
	}
	if !confirmRead(eng.SelectionCost(steps), yes) {
		return nil
	}
	if tree.HasIncrementals() {
		fmt.Println(fileLevelDeletionNote)
	}
	n, err := eng.ExtractSelection(steps, dest, logf)
	if err != nil {
		return err
	}
	fmt.Printf("recovered %d file(s) from %d archive(s) into %s\n", n, len(steps), dest)
	return nil
}

// fileLevelDeletionNote warns that file-level recovery merges the restore chain as a
// union and so is not deletion-accurate: a file deleted before the as-of date can
// reappear (GNU tar records deletions in its snapshot, not the member index). A
// whole-DLE `--all` restore replays the chain with listed-incremental extraction and
// is deletion-accurate.
const fileLevelDeletionNote = "note: file-level recovery is not deletion-accurate — a file deleted before the as-of date may reappear; use `nb recover --all` for a deletion-accurate whole-DLE restore."

// pathRootHint reminds a user that recover paths are relative to the DLE's backed-up
// root, the common reason a real absolute source path is "not found". It fires only
// for an absolute-looking path (the natural mistake), so a simple relative typo is
// left with the plain message.
func pathRootHint(p string) string {
	if strings.HasPrefix(strings.TrimSpace(p), "/") {
		return " (paths are relative to the DLE's backed-up root — e.g. /etc, not the source's full absolute path)"
	}
	return ""
}

// recoverDate parses the as-of date, defaulting to today.
func recoverDate(s string) (string, error) {
	d, err := ParseDate(s)
	if err != nil {
		return "", err
	}
	return record.DateString(d), nil
}

// recoverAsOf resolves the --date / --time flags into the string recovery.AsOf
// understands: a bare YYYY-MM-DD (whole day) or a 'YYYY-MM-DD HH[:MM[:SS]]' instant.
// The two flags are mutually exclusive — --time is the point-in-time form that can
// reach an earlier same-day run, --date selects the whole day.
func recoverAsOf(dateStr, timeStr string) (string, error) {
	if timeStr != "" {
		if dateStr != "" {
			return "", fmt.Errorf("--date and --time are mutually exclusive (use --time for a point-in-time, --date for a whole day)")
		}
		return validateAsOfTime(timeStr)
	}
	return recoverDate(dateStr)
}

// validateAsOfTime checks an as-of time value parses (UTC) at day, hour, minute, or
// second precision and returns it normalized. A bare date is accepted too. The
// accepted layouts live with the resolution (recovery.ValidateAsOf), so the flag
// and AsOf can never drift apart.
func validateAsOfTime(s string) (string, error) {
	return recovery.ValidateAsOf(s)
}

// printListing renders one directory's entries, directories suffixed with "/".
func printListing(n *recovery.Node) {
	if !n.IsDir() {
		fmt.Printf("  %s\n", n.Name())
		return
	}
	children := n.Children()
	if len(children) == 0 {
		fmt.Println("  (empty)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, c := range children {
		name := c.Name()
		if c.IsDir() {
			name += "/"
		}
		fmt.Fprintf(tw, "  %s\n", name)
	}
	tw.Flush()
}
