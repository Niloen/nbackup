package cli

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// newRecoverCmd implements `nb recover`: browse a DLE's files as of a date and
// recover a selection. With no selection flags it opens an
// interactive shell (setdate/cd/ls/add/extract); with --path it runs one-shot,
// and with --list it just prints a listing.
func newRecoverCmd(a *app) *cobra.Command {
	var ra recoverArgs
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Browse a date and recover selected files, or restore a whole DLE",
		Long: "Recover from backups as they stood on a date. Three modes:\n\n" +
			"  • interactive (no flags): a shell to browse and pick files — setdate, setdisk," +
			" disks, cd, ls, add, delete, list, extract.\n" +
			"  • file-level (--path / --list): recover the named files/dirs in one shot, or just" +
			" print a listing. Selected-file recovery never deletes. A --path that names an" +
			" INVENTORY UNIT (see --inventory; e.g. public.users) exports the unit in its useful" +
			" form instead — a table becomes <unit>.sql, produced by restoring the DLE to scratch" +
			" and dumping with the database's own tools; importing the SQL stays your act.\n" +
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
			cfg, err := a.loadOrDefaultCatalog()
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
			if ra.inventory {
				if ra.all || ra.list || len(ra.paths) > 0 {
					return fmt.Errorf("--inventory is its own mode and cannot be combined with --all/--list/--path")
				}
				return runRecoverInventory(eng, ra)
			}
			if ra.all {
				if ra.list || len(ra.paths) > 0 {
					return fmt.Errorf("--all restores the whole DLE and cannot be combined with --path/--list")
				}
				return runRecoverRestore(eng, ra, a.logf())
			}
			if ra.list || len(ra.paths) > 0 {
				return runRecoverBatch(eng, ra, a.logf())
			}
			return runRecoverShell(eng, ra.dle, ra.date, ra.time, ra.dest)
		},
	}
	cmd.Flags().StringVar(&ra.dle, "dle", "", "DLE to recover from (with --all, omit to restore every DLE)")
	cmd.Flags().StringVar(&ra.date, "date", "", "as-of date YYYY-MM-DD (default today); resolves to the most recent run on or before that day")
	cmd.Flags().StringVar(&ra.time, "time", "", "as-of point-in-time 'YYYY-MM-DD HH[:MM[:SS]]' (UTC); resolves to the most recent run committed in or before that period — reaches an earlier same-day run. Mutually exclusive with --date")
	cmd.Flags().StringArrayVar(&ra.paths, "path", nil, "file/dir to recover, or an inventory unit name to export (repeatable); a file lands as the file, a unit (e.g. public.users) as its useful form — a table exports as <unit>.sql via a scratch restore; non-interactive")
	cmd.Flags().StringVar(&ra.dest, "dest", "", "destination directory for recovered files")
	cmd.Flags().BoolVar(&ra.list, "list", false, "print a listing of --path (or the root) and exit")
	cmd.Flags().BoolVar(&ra.inventory, "inventory", false, "print the DLE's content inventory as of the date — the named units the archiver reported at dump time (postgres: tables with sizes) — and exit")
	cmd.Flags().BoolVar(&ra.all, "all", false, "restore the whole DLE (deletion-accurate) as of the date into --dest")
	cmd.Flags().BoolVar(&ra.force, "force", false, "with --all, restore into a non-empty --dest (its contents are pruned to match the backup)")
	cmd.Flags().BoolVar(&ra.yes, "yes", false, "skip the egress-cost confirmation when reading from a cloud/cold medium")
	cmd.Flags().StringVar(&ra.to, "to", "", "with --all, restore onto a remote client: host:path (host must be in the config's hosts:); tar runs on the client, and for an encrypt.at: client DLE so does decryption — the key stays on the client")
	cmd.Flags().StringVar(&ra.from, "from", "", "with --all, read from this medium's copy specifically (e.g. the offsite tape) instead of auto-selecting any available copy")
	return cmd
}

// recoverArgs carries `nb recover`'s flag set, bound once in newRecoverCmd and
// passed whole to the mode handlers instead of a positional parade of ten params.
type recoverArgs struct {
	dle, date, time                  string // selection: which DLE, as of when
	dest, to, from                   string // where to restore, and from which medium's copy
	paths                            []string
	list, inventory, all, force, yes bool
}

// runRecoverInventory prints a DLE's content inventory as of the date: the
// named units the archiver reported at dump time, sized — the "what tables are
// in this backup" report. The vocabulary in the unit paths is the archiver's
// own ("tables/…" for postgres); this mode only renders it.
func runRecoverInventory(eng *engine.Engine, ra recoverArgs) error {
	if ra.dle == "" {
		return fmt.Errorf("--inventory needs --dle (it is one backup's content report); known: %s", strings.Join(eng.DLEDisplay(), ", "))
	}
	slug, ok := eng.ResolveDLE(ra.dle)
	if !ok {
		return fmt.Errorf("unknown DLE %q; known: %s", ra.dle, strings.Join(eng.DLEDisplay(), ", "))
	}
	asOf, err := recoverAsOf(ra.date, ra.time)
	if err != nil {
		return err
	}
	units, run, err := eng.Inventory(slug, asOf)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		fmt.Printf("no inventory recorded for %s as of %s (run %s) — its archiver reports none\n", eng.DisplayDLE(slug), asOf, run)
		return nil
	}
	printUnits(units)
	fmt.Printf("%d units · run %s\n", len(units), run)
	return nil
}

// printUnits renders a unit list as aligned rows: path, size, file count.
func printUnits(units []record.Unit) {
	width := 0
	for _, u := range units {
		if len(u.Path) > width {
			width = len(u.Path)
		}
	}
	for _, u := range units {
		size := "-"
		if u.Size > 0 {
			size = sizeutil.FormatBytes(u.Size)
		}
		files := ""
		if n := len(u.Members); n > 0 {
			files = fmt.Sprintf("  (%d file(s))", n)
		}
		fmt.Printf("  %-*s  %10s%s\n", width, u.Path, size, files)
	}
}

// runRecoverRestore performs a whole-DLE, deletion-accurate restore as of a date —
// the folded-in `nb restore`. With --dle it restores that DLE; without, every DLE
// in the catalog, each into its own subdirectory of dest.
func runRecoverRestore(eng *engine.Engine, ra recoverArgs, logf engine.Logf) error {
	asOf, err := recoverAsOf(ra.date, ra.time)
	if err != nil {
		return err
	}
	dest := ra.dest
	// --to host:path restores onto a remote client (tar runs there) instead of a local
	// --dest. The two are mutually exclusive: --to carries its own destination path.
	var toHost, toPath string
	if ra.to != "" {
		if dest != "" {
			return fmt.Errorf("--to and --dest are mutually exclusive (--to host:path carries the destination)")
		}
		h, p, ok := strings.Cut(ra.to, ":")
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
	specified := ra.dle != ""
	if specified {
		slug, ok := eng.ResolveDLE(ra.dle)
		if !ok {
			return fmt.Errorf("unknown DLE %q; known: %s", ra.dle, strings.Join(eng.DLEDisplay(), ", "))
		}
		dles = []string{slug}
	} else {
		dles = eng.DLENames()
		if len(dles) == 0 {
			return fmt.Errorf("no DLEs in the catalog")
		}
	}
	if !confirmRead(eng.RestoreCost(dles, asOf), ra.yes) {
		return nil
	}
	// Restoring every DLE lays each under dest/<dle>. dest itself is ours to
	// create: a tree archiver's extraction would anyway (MkdirAll), but an
	// opaque-destination archiver (pipe) writes dest/<dle> as a single path and
	// must find its parent in place.
	if !specified && toHost == "" {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
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
				return eng.RestoreAsOfTo(name, asOf, toHost, out, ra.from, logf)
			}
			return eng.RestoreAsOf(name, asOf, out, ra.from, ra.force, logf)
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
func runRecoverBatch(eng *engine.Engine, ra recoverArgs, logf engine.Logf) error {
	asOf, err := recoverAsOf(ra.date, ra.time)
	if err != nil {
		return err
	}
	if ra.dle == "" {
		return fmt.Errorf("--dle is required (known: %s)", strings.Join(eng.DLEDisplay(), ", "))
	}
	slug, ok := eng.ResolveDLE(ra.dle)
	if !ok {
		return fmt.Errorf("unknown DLE %q; known: %s", ra.dle, strings.Join(eng.DLEDisplay(), ", "))
	}
	tree, err := eng.OpenRecover(slug, asOf)
	if err != nil {
		return err
	}

	if ra.list {
		target := "/"
		if len(ra.paths) > 0 {
			target = ra.paths[0]
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

	if ra.dest == "" {
		return fmt.Errorf("--dest is required to recover files")
	}
	// One pointing rule: an exact tree path is that FILE; anything else resolves
	// against the inventory's unit names — pointing at a thing yields the thing
	// in its useful form (a table exports as SQL). Precedence is deterministic,
	// and unit names are disjoint from member paths by the archiver's contract.
	var filePaths, unitNames []string
	var units []record.Unit
	for _, p := range ra.paths {
		if _, ok := tree.Lookup(p); ok {
			filePaths = append(filePaths, p)
			continue
		}
		if units == nil {
			units, _, _ = eng.Inventory(slug, asOf)
		}
		matches := recovery.MatchUnits(units, p)
		switch len(matches) {
		case 0:
			return fmt.Errorf("not found: %s%s (neither a backed-up path nor an inventory unit — see --list and --inventory)", p, pathRootHint(p))
		case 1:
			fmt.Printf("matched unit %s — will export as %s.sql\n", matches[0].Path, matches[0].Path)
			unitNames = append(unitNames, matches[0].Path)
		default:
			var cands []string
			for _, m := range matches {
				cands = append(cands, m.Path)
			}
			return fmt.Errorf("%q matches %d units — use a fuller name: %s", p, len(matches), strings.Join(cands, ", "))
		}
	}

	recovered, exported := 0, 0
	if len(filePaths) > 0 {
		steps, asms, err := tree.Collect(filePaths)
		if err != nil {
			return err
		}
		rows, est := eng.SelectionPlan(steps)
		printReadPlan(rows)
		if !confirmRead(est, ra.yes) {
			return nil
		}
		if tree.HasIncrementals() {
			fmt.Println(fileLevelDeletionNote)
		}
		n, archives, err := eng.ExtractSelection(steps, asms, ra.dest, logf, newExtractProgress(est.Bytes))
		if err != nil {
			return err
		}
		recovered = n
		fmt.Printf("recovered %d file(s) from %d archive(s) into %s\n", n, archives, ra.dest)
	}
	if len(unitNames) > 0 {
		// The honest cost of exporting from a physical backup is a whole-DLE
		// scratch restore — the same read --all would take, priced and confirmed
		// the same way.
		if !confirmRead(eng.RestoreCost([]string{slug}, asOf), ra.yes) {
			return nil
		}
		written, err := eng.ExportUnits(slug, asOf, unitNames, ra.dest, logf)
		if err != nil {
			return err
		}
		exported = len(written)
		for _, w := range written {
			fmt.Printf("wrote %s\n", w)
		}
	}
	if recovered == 0 && exported == 0 {
		return fmt.Errorf("nothing selected")
	}
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
// reach an earlier same-day run, --date selects the whole day. The accepted --time
// layouts live with the resolution (recovery.ValidateAsOf), so the flag and AsOf
// can never drift apart.
func recoverAsOf(dateStr, timeStr string) (string, error) {
	if timeStr != "" {
		if dateStr != "" {
			return "", fmt.Errorf("--date and --time are mutually exclusive (use --time for a point-in-time, --date for a whole day)")
		}
		return recovery.ValidateAsOf(timeStr)
	}
	return recoverDate(dateStr)
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
	tw := newTab(os.Stdout)
	for _, c := range children {
		name := c.Name()
		if c.IsDir() {
			name += "/"
		}
		fmt.Fprintf(tw, "  %s\n", name)
	}
	tw.Flush()
}
