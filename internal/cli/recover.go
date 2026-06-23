package cli

import (
	"fmt"
	"os"
	"path"
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
	var listOnly bool
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Browse backups as of a date and recover selected files (amrecover-style)",
		Long: "Browse a DLE's filesystem as it stood on a date and recover individual files or" +
			" directories. The view merges the restore chain (the full plus later incrementals up" +
			" to the date), so each file is recovered from the archive that last held it.\n\n" +
			"With no --path/--list it opens an interactive shell: setdate, setdisk, cd, ls, add," +
			" delete, list, extract. With --path it recovers that selection in one shot; with" +
			" --list it prints a directory listing and exits. Paths are relative to the DLE root.",
		Example: "  nb recover\n" +
			"  nb recover --dle app01-home --date 2026-06-20 --list --path /etc\n" +
			"  nb recover --dle app01-home --date 2026-06-20 --path /etc/hosts --path /etc/nginx --dest /tmp/out",
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
			if listOnly || len(paths) > 0 {
				return runRecoverBatch(eng, dleName, dateStr, paths, dest, listOnly, a.logf())
			}
			return runRecoverShell(eng, dleName, dateStr, dest)
		},
	}
	cmd.Flags().StringVar(&dleName, "dle", "", "DLE to recover from")
	cmd.Flags().StringVar(&dateStr, "date", "", "as-of date YYYY-MM-DD (default today)")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "file/dir to recover (repeatable); non-interactive")
	cmd.Flags().StringVar(&dest, "dest", "", "destination directory for recovered files")
	cmd.Flags().BoolVar(&listOnly, "list", false, "print a listing of --path (or the root) and exit")
	return cmd
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
			return fmt.Errorf("not found: %s", target)
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
		return err
	}
	n, err := eng.ExtractSelection(steps, dest, logf)
	if err != nil {
		return err
	}
	fmt.Printf("recovered %d entr(ies) from %d archive(s) into %s\n", n, len(steps), dest)
	return nil
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
	} else {
		fmt.Printf("as of %s; set a disk with 'setdisk <dle>'. Known DLEs: %s\n",
			sh.date, strings.Join(sh.eng.DLENames(), ", "))
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
