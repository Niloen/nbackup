package cli

import (
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/recovery"
)

// newRecoverCmd implements `nb recover`: browse a DLE's files as of a date and
// recover a selection — NBackup's amrecover. With no selection flags it opens an
// interactive shell (setdate/cd/ls/add/extract); with --path it runs one-shot,
// and with --list it just prints a listing.
func newRecoverCmd(a *app) *cobra.Command {
	var dleName, dateStr, dest string
	var paths []string
	var listOnly, all, force bool
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Browse a date and recover selected files, or restore a whole DLE (amrecover-style)",
		Long: "Recover from backups as they stood on a date. Three modes:\n\n" +
			"  • interactive (no flags): a shell to browse and pick files — setdate, setdisk," +
			" disks, cd, ls, add, delete, list, extract.\n" +
			"  • file-level (--path / --list): recover the named files/dirs in one shot, or just" +
			" print a listing. Selected-file recovery never deletes.\n" +
			"  • whole-DLE (--all): rebuild an entire DLE (or every DLE) as of the date into --dest," +
			" replaying the full plus later incrementals so deletions are applied. This prunes the" +
			" destination to match the backup, so --dest must be empty unless --force.\n\n" +
			"Paths are relative to the DLE root. A bare --date resolves to the most recent slot on" +
			" or before it — the same slot the browse view and --all restore both use.",
		Example: "  nb recover\n" +
			"  nb recover --dle app01-home --date 2026-06-20 --list --path /etc\n" +
			"  nb recover --dle app01-home --date 2026-06-20 --path /etc/hosts --dest /tmp/out\n" +
			"  nb recover --dle app01-home --date 2026-06-20 --all --dest /tmp/out",
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
			eng.SetOperator(stdinOperator{})
			if all {
				if listOnly || len(paths) > 0 {
					return fmt.Errorf("--all restores the whole DLE and cannot be combined with --path/--list")
				}
				return runRecoverRestore(eng, dleName, dateStr, dest, force, a.logf())
			}
			if listOnly || len(paths) > 0 {
				return runRecoverBatch(eng, dleName, dateStr, paths, dest, listOnly, a.logf())
			}
			return runRecoverShell(eng, dleName, dateStr, dest)
		},
	}
	cmd.Flags().StringVar(&dleName, "dle", "", "DLE to recover from (with --all, omit to restore every DLE)")
	cmd.Flags().StringVar(&dateStr, "date", "", "as-of date YYYY-MM-DD (default today)")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "file/dir to recover (repeatable); non-interactive")
	cmd.Flags().StringVar(&dest, "dest", "", "destination directory for recovered files")
	cmd.Flags().BoolVar(&listOnly, "list", false, "print a listing of --path (or the root) and exit")
	cmd.Flags().BoolVar(&all, "all", false, "restore the whole DLE (deletion-accurate) as of the date into --dest")
	cmd.Flags().BoolVar(&force, "force", false, "with --all, restore into a non-empty --dest (its contents are pruned to match the backup)")
	return cmd
}

// runRecoverRestore performs a whole-DLE, deletion-accurate restore as of a date —
// the folded-in `nb restore`. With --dle it restores that DLE; without, every DLE
// in the catalog, each into its own subdirectory of dest.
func runRecoverRestore(eng *engine.Engine, dleName, dateStr, dest string, force bool, logf engine.Logf) error {
	asOf, err := recoverDate(dateStr)
	if err != nil {
		return err
	}
	if dest == "" {
		return fmt.Errorf("--dest is required for --all (whole-DLE restore)")
	}
	var dles []string
	if dleName != "" {
		if !slices.Contains(eng.DLENames(), dleName) {
			return fmt.Errorf("unknown DLE %q; known: %s", dleName, strings.Join(eng.DLENames(), ", "))
		}
		dles = []string{dleName}
	} else {
		dles = eng.DLENames()
		if len(dles) == 0 {
			return fmt.Errorf("no DLEs in the catalog")
		}
	}
	for _, name := range dles {
		out := dest
		if len(dles) > 1 {
			out = path.Join(dest, name)
		}
		fmt.Printf("restoring DLE %s as of %s -> %s\n", name, asOf, out)
		if err := eng.RestoreAsOf(name, asOf, out, force, logf); err != nil {
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
func runRecoverBatch(eng *engine.Engine, dleName, dateStr string, paths []string, dest string, listOnly bool, logf engine.Logf) error {
	asOf, err := recoverDate(dateStr)
	if err != nil {
		return err
	}
	if dleName == "" {
		return fmt.Errorf("--dle is required (known: %s)", strings.Join(eng.DLENames(), ", "))
	}
	if !slices.Contains(eng.DLENames(), dleName) {
		return fmt.Errorf("unknown DLE %q; known: %s", dleName, strings.Join(eng.DLENames(), ", "))
	}
	tree, err := eng.OpenRecover(dleName, asOf)
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
		fmt.Printf("# %s as of %s (%s)\n", dleName, tree.AsOf, tree.TargetSlot)
		printListing(n)
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
	fmt.Println(fileLevelDeletionNote)
	n, err := eng.ExtractSelection(steps, dest, logf)
	if err != nil {
		return err
	}
	fmt.Printf("recovered %d entr(ies) from %d archive(s) into %s\n", n, len(steps), dest)
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
	return d.Format("2006-01-02"), nil
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

// recoverShell is the interactive amrecover-style session state.
type recoverShell struct {
	eng  *engine.Engine
	dle  string
	date string // YYYY-MM-DD
	tree *recovery.Tree
	cwd  string          // clean path from the DLE root
	sel  map[string]bool // selected clean paths
	dest string
}

// runRecoverShell drives the interactive recovery prompt. It reuses the shared
// stdinReader so it coexists with operator swap prompts during extraction.
func runRecoverShell(eng *engine.Engine, dleName, dateStr, dest string) error {
	date, err := recoverDate(dateStr)
	if err != nil {
		return err
	}
	sh := &recoverShell{eng: eng, dle: dleName, date: date, sel: map[string]bool{}, dest: dest}
	if dleName != "" {
		if err := sh.reload(); err != nil {
			fmt.Printf("note: %v\n", err)
		}
	}
	fmt.Println("nb recover — type 'help' for commands, 'quit' to exit.")
	sh.banner()
	for {
		fmt.Print(sh.prompt())
		line, rerr := stdinReader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			if rerr != nil {
				fmt.Println()
				return nil // EOF
			}
			continue
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
	loc := "/"
	if sh.cwd != "" {
		loc = "/" + sh.cwd
	}
	if sh.tree == nil {
		return "recover> "
	}
	return fmt.Sprintf("recover %s:%s> ", sh.dle, loc)
}

func (sh *recoverShell) banner() {
	if sh.tree != nil {
		fmt.Printf("disk %q as of %s (resolved to %s)\n", sh.dle, sh.date, sh.tree.TargetSlot)
		return
	}
	fmt.Printf("as of %s — no disk selected. Pick one with 'setdisk <dle>' ('disks' lists them).\n", sh.date)
	sh.listDisks()
}

// listDisks prints the DLEs available to recover from, one per line (they are
// descriptive, often long, so a comma list is unreadable).
func (sh *recoverShell) listDisks() {
	names := sh.eng.DLENames()
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
	case "setdisk", "disk", "sethost":
		sh.setDisk(args)
	case "disks", "disklist", "lsdisk":
		sh.listDisks()
	case "ls", "dir":
		sh.ls(args)
	case "cd":
		sh.cd(args)
	case "pwd":
		fmt.Println("/" + sh.cwd)
	case "add":
		sh.add(args)
	case "delete", "rm", "del":
		sh.del(args)
	case "list", "selected":
		sh.listSelected()
	case "clear":
		sh.sel = map[string]bool{}
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

func (sh *recoverShell) reload() error {
	if sh.dle == "" {
		return fmt.Errorf("no disk set")
	}
	tree, err := sh.eng.OpenRecover(sh.dle, sh.date)
	if err != nil {
		sh.tree = nil
		return err
	}
	sh.tree = tree
	sh.cwd = ""
	return nil
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
	sh.date = d
	if sh.dle != "" {
		if err := sh.reload(); err != nil {
			fmt.Printf("note: %v\n", err)
			return
		}
	}
	sh.banner()
}

func (sh *recoverShell) setDisk(args []string) {
	if len(args) != 1 {
		fmt.Printf("usage: setdisk <dle>; known: %s\n", strings.Join(sh.eng.DLENames(), ", "))
		return
	}
	sh.dle = args[0]
	sh.sel = map[string]bool{}
	if err := sh.reload(); err != nil {
		fmt.Printf("note: %v\n", err)
		return
	}
	sh.banner()
}

func (sh *recoverShell) ls(args []string) {
	if sh.tree == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	target := sh.cwd
	if len(args) == 1 {
		target = sh.resolve(args[0])
	}
	n, ok := sh.tree.Lookup(target)
	if !ok {
		fmt.Printf("not found: /%s\n", target)
		return
	}
	printListing(n)
}

func (sh *recoverShell) cd(args []string) {
	if sh.tree == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	if len(args) == 0 {
		sh.cwd = ""
		return
	}
	target := sh.resolve(args[0])
	n, ok := sh.tree.Lookup(target)
	if !ok {
		fmt.Printf("not found: /%s\n", target)
		return
	}
	if !n.IsDir() {
		fmt.Printf("not a directory: /%s\n", target)
		return
	}
	sh.cwd = target
}

func (sh *recoverShell) add(args []string) {
	if sh.tree == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	if len(args) == 0 {
		fmt.Println("usage: add <path>...")
		return
	}
	for _, p := range args {
		target := sh.resolve(p)
		if _, ok := sh.tree.Lookup(target); !ok {
			fmt.Printf("not found: /%s\n", target)
			continue
		}
		sh.sel[target] = true
		fmt.Printf("added /%s\n", target)
	}
}

func (sh *recoverShell) del(args []string) {
	for _, p := range args {
		target := sh.resolve(p)
		if sh.sel[target] {
			delete(sh.sel, target)
			fmt.Printf("removed /%s\n", target)
		}
	}
}

func (sh *recoverShell) listSelected() {
	if len(sh.sel) == 0 {
		fmt.Println("(no files selected)")
		return
	}
	for _, p := range sortedSet(sh.sel) {
		fmt.Printf("  /%s\n", p)
	}
}

func (sh *recoverShell) extract(args []string) {
	if sh.tree == nil {
		fmt.Println("set a disk first (setdisk <dle>)")
		return
	}
	if len(sh.sel) == 0 {
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
	steps, err := sh.tree.Collect(sortedSet(sh.sel))
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Println(fileLevelDeletionNote)
	n, err := sh.eng.ExtractSelection(steps, sh.dest, logfStdout)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Printf("recovered %d entr(ies) from %d archive(s) into %s\n", n, len(steps), sh.dest)
}

// resolve turns a user-typed path (absolute "/a/b" or relative to cwd, with "."
// and "..") into a clean path from the DLE root.
func (sh *recoverShell) resolve(arg string) string {
	base := "/" + sh.cwd
	if strings.HasPrefix(arg, "/") {
		base = "/"
	}
	return strings.TrimPrefix(path.Clean(base+"/"+arg), "/")
}

func recoverHelp() {
	fmt.Print(`commands:
  disks                list the DLEs available to recover from
  setdisk <dle>        choose the DLE to browse (alias: disk)
  setdate <date>       set the as-of date (YYYY-MM-DD), then rebrowse
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

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
