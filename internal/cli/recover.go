package cli

import (
	"fmt"
	"os"
	"path"
	"strings"
	"text/tabwriter"
	"time"

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
			eng, err := newEngine(cfg)
			if err != nil {
				return err
			}
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
// second precision and returns it normalized. A bare date is accepted too.
func validateAsOfTime(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02 15", "2006-01-02"} {
		if _, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return s, nil
		}
	}
	return "", fmt.Errorf("invalid time %q: want 'YYYY-MM-DD HH[:MM[:SS]]' (UTC) or YYYY-MM-DD", s)
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

// recoverShell is the interactive recovery session: the terminal I/O (prompt,
// dispatch, printing) wrapped around a pure recovery.Session that holds the browse
// tree, current directory, and selection.
type recoverShell struct {
	eng   *engine.Engine
	dle   string
	date  string // YYYY-MM-DD
	sess  *recovery.Session
	dest  string
	noted bool // whether the file-level deletion note has been shown this session
}

// runRecoverShell drives the interactive recovery prompt. It reuses the shared
// stdinReader so it coexists with operator swap prompts during extraction.
func runRecoverShell(eng *engine.Engine, dleName, dateStr, timeStr, dest string) error {
	asOf, err := recoverAsOf(dateStr, timeStr)
	if err != nil {
		return err
	}
	sh := &recoverShell{eng: eng, date: asOf, dest: dest}
	if dleName != "" {
		if slug, ok := eng.ResolveDLE(dleName); ok {
			sh.dle = slug
			if err := sh.reload(); err != nil {
				fmt.Printf("note: %v\n", err)
			}
		} else {
			fmt.Printf("note: unknown DLE %q — pick one with 'setdisk <dle>'\n", dleName)
		}
	}
	fmt.Println("nb recover — type 'help' for commands, 'quit' to exit.")
	sh.banner()
	tty := stdinIsTerminal() // suppress the prompt when stdin is piped (avoids interleaved echo)
	for {
		if tty {
			fmt.Print(sh.prompt())
		}
		line, rerr := stdinReader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			if rerr != nil {
				fmt.Println()
				return nil // EOF
			}
			continue
		}
		if !tty {
			// Piped/scripted input isn't echoed by a terminal, so a transcript would
			// show output with no commands. Echo the prompt + command ourselves so a
			// scripted session reads top-to-bottom.
			fmt.Printf("%s%s\n", sh.prompt(), line)
		}
		fields := strings.Fields(line)
		if sh.dispatch(fields[0], fields[1:]) {
			return nil
		}
		if rerr != nil {
			return nil
		}
	}
}

func (sh *recoverShell) prompt() string {
	if sh.sess == nil {
		return "recover> "
	}
	loc := "/"
	if cwd := sh.sess.Cwd(); cwd != "" {
		loc = "/" + cwd
	}
	return fmt.Sprintf("recover %s:%s> ", sh.eng.DisplayDLE(sh.dle), loc)
}

func (sh *recoverShell) banner() {
	if sh.sess != nil {
		fmt.Printf("disk %q as of %s (resolved to %s)\n", sh.eng.DisplayDLE(sh.dle), sh.date, sh.sess.Tree().TargetRun)
		sh.noteDeletionOnce()
		return
	}
	fmt.Printf("as of %s — no disk selected. Pick one with 'setdisk <dle>' ('disks' lists them).\n", sh.date)
	sh.listDisks()
}

// listDisks prints the DLEs available to recover from, one per line (they are
// descriptive, often long, so a comma list is unreadable).
func (sh *recoverShell) listDisks() {
	names := sh.eng.DLEDisplay()
	if len(names) == 0 {
		fmt.Println("  (no disks in the catalog — is the config/catalog correct?)")
		return
	}
	fmt.Println("available disks:")
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
}

// dispatch runs one command; it returns true when the session should end.
func (sh *recoverShell) dispatch(cmd string, args []string) (quit bool) {
	switch cmd {
	case "help", "?":
		recoverHelp()
	case "quit", "exit", "q":
		return true
	case "setdate":
		sh.setDate(args)
	case "settime":
		sh.setTime(args)
	case "setdisk", "disk", "sethost":
		sh.setDisk(args)
	case "disks", "disklist", "lsdisk":
		sh.listDisks()
	case "ls", "dir":
		sh.ls(args)
	case "cd":
		sh.cd(args)
	case "pwd":
		fmt.Println("/" + sh.cwd())
	case "add":
		sh.add(args)
	case "delete", "rm", "del":
		sh.del(args)
	case "list", "selected":
		sh.listSelected()
	case "clear":
		sh.sess.Clear()
		fmt.Println("selection cleared")
	case "dest", "setdest", "lcd":
		if len(args) == 1 {
			sh.dest = args[0]
		}
		fmt.Printf("dest: %s\n", orDash(sh.dest))
	case "extract", "recover":
		sh.extract(args)
	default:
		fmt.Printf("unknown command %q (try 'help')\n", cmd)
	}
	return false
}

// cwd is the session's current directory, or "" before a disk is set.
func (sh *recoverShell) cwd() string {
	if sh.sess == nil {
		return ""
	}
	return sh.sess.Cwd()
}

func (sh *recoverShell) reload() error {
	if sh.dle == "" {
		return fmt.Errorf("no disk set")
	}
	tree, err := sh.eng.OpenRecover(sh.dle, sh.date)
	if err != nil {
		sh.sess = nil
		return err
	}
	sh.sess = recovery.NewSession(tree)
	return nil
}

// rebrowse re-opens the current disk as of a candidate date, committing it as the
// session's as-of date only when a backup exists then. A date with no backup
// leaves the current selection, browse position, and as-of date untouched — so a
// mistyped or backup-less date never silently drops the disk selection.
func (sh *recoverShell) rebrowse(date string) error {
	tree, err := sh.eng.OpenRecover(sh.dle, date)
	if err != nil {
		return err
	}
	sh.date = date
	sh.sess = recovery.NewSession(tree)
	return nil
}

// noteDeletionOnce prints the file-level deletion caveat the first time a browsed
// disk has incrementals, then stays quiet — so a session that rebrowses (setdate,
// settime) and then extracts doesn't repeat the same warning.
func (sh *recoverShell) noteDeletionOnce() {
	if sh.noted || sh.sess == nil || !sh.sess.Tree().HasIncrementals() {
		return
	}
	fmt.Println(fileLevelDeletionNote)
	sh.noted = true
}

func (sh *recoverShell) setDate(args []string) {
	if len(args) != 1 {
		fmt.Println("usage: setdate YYYY-MM-DD")
		return
	}
	d, err := recoverDate(args[0])
	if err != nil {
		fmt.Printf("bad date: %v\n", err)
		return
	}
	if sh.dle == "" {
		sh.date = d
		sh.banner()
		return
	}
	if err := sh.rebrowse(d); err != nil {
		// A date with no backup is reported but leaves the current disk,
		// selection, and browse position intact — like a bad-format date.
		fmt.Printf("note: %v\n", err)
		return
	}
	sh.banner()
}

// setTime sets the as-of point-in-time, then rebrowses. Unlike setdate it accepts a
// time so an earlier same-day run is reachable; the args are rejoined since the
// "YYYY-MM-DD HH:MM" form contains a space the shell splits on.
func (sh *recoverShell) setTime(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: settime YYYY-MM-DD HH[:MM[:SS]]")
		return
	}
	when, err := validateAsOfTime(strings.Join(args, " "))
	if err != nil {
		fmt.Printf("bad time: %v\n", err)
		return
	}
	if sh.dle == "" {
		sh.date = when
		sh.banner()
		return
	}
	if err := sh.rebrowse(when); err != nil {
		// A time with no backup is reported but leaves the current disk,
		// selection, and browse position intact — like a bad-format time.
		fmt.Printf("note: %v\n", err)
		return
	}
	sh.banner()
}

func (sh *recoverShell) setDisk(args []string) {
	if len(args) != 1 {
		fmt.Printf("usage: setdisk <dle>; known: %s\n", strings.Join(sh.eng.DLEDisplay(), ", "))
		return
	}
	slug, ok := sh.eng.ResolveDLE(args[0])
	if !ok {
		fmt.Printf("unknown disk %q; known: %s\n", args[0], strings.Join(sh.eng.DLEDisplay(), ", "))
		return
	}
	sh.dle = slug
	if err := sh.reload(); err != nil { // a fresh session starts with an empty selection
		fmt.Printf("note: %v\n", err)
		return
	}
	sh.banner()
}

func (sh *recoverShell) ls(args []string) {
	if sh.sess == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	n, ok := sh.sess.Lookup(arg)
	if !ok {
		fmt.Printf("not found: /%s\n", sh.sess.Resolve(arg))
		return
	}
	printListing(n)
}

func (sh *recoverShell) cd(args []string) {
	if sh.sess == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	if err := sh.sess.Cd(arg); err != nil {
		fmt.Println(err)
	}
}

func (sh *recoverShell) add(args []string) {
	if sh.sess == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	if len(args) == 0 {
		fmt.Println("usage: add <path>...")
		return
	}
	added, notFound := sh.sess.Add(args)
	for _, p := range added {
		fmt.Printf("added /%s\n", p)
	}
	for _, p := range notFound {
		fmt.Printf("not found: /%s\n", p)
	}
}

func (sh *recoverShell) del(args []string) {
	if sh.sess == nil {
		return
	}
	removed := sh.sess.Remove(args)
	for _, p := range removed {
		fmt.Printf("removed /%s\n", p)
	}
	// Give feedback on a no-op delete rather than staying silent, so a mistyped or
	// already-unselected path is obviously a no-op, not a success.
	if len(removed) == 0 && len(args) > 0 {
		fmt.Printf("not in selection: %s\n", strings.Join(args, " "))
	}
}

func (sh *recoverShell) listSelected() {
	if sh.sess == nil {
		fmt.Println("(no files selected)")
		return
	}
	sel := sh.sess.Selection()
	if len(sel) == 0 {
		fmt.Println("(no files selected)")
		return
	}
	for _, p := range sel {
		fmt.Printf("  /%s\n", p)
	}
}

func (sh *recoverShell) extract(args []string) {
	if sh.sess == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	if len(sh.sess.Selection()) == 0 {
		fmt.Println("nothing selected (use 'add <path>')")
		return
	}
	if len(args) == 1 {
		sh.dest = args[0]
	}
	if sh.dest == "" {
		fmt.Print("destination directory: ")
		line, _ := stdinReader.ReadString('\n')
		sh.dest = strings.TrimSpace(line)
	}
	if sh.dest == "" {
		fmt.Println("no destination; aborted")
		return
	}
	steps, err := sh.sess.CollectSelection()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	if !confirmRead(sh.eng.SelectionCost(steps), false) {
		return
	}
	sh.noteDeletionOnce()
	n, err := sh.eng.ExtractSelection(steps, sh.dest, logfStdout)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Printf("recovered %d file(s) from %d archive(s) into %s\n", n, len(steps), sh.dest)
}

func recoverHelp() {
	fmt.Print(`commands:
  disks                list the DLEs available to recover from
  setdisk <dle>        choose the DLE to browse (alias: disk)
  setdate <date>       set the as-of date (YYYY-MM-DD), then rebrowse
  settime <date time>  set the as-of point-in-time (YYYY-MM-DD HH[:MM[:SS]], UTC),
                       reaching an earlier same-day run, then rebrowse
  ls [path]            list a directory
  cd [path]            change directory (.. and absolute /paths work)
  pwd                  print the current directory
  add <path>...        mark files/dirs for recovery
  delete <path>...     unmark (alias: rm)
  list                 show the current selection
  clear                clear the selection
  dest <dir>           set the destination directory
  extract [dir]        recover the selection into the destination
  help                 this help
  quit                 leave
`)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
