// recover_shell.go is `nb recover`'s interactive mode: the terminal I/O (prompt,
// dispatch, printing) wrapped around a pure recovery.Session that holds the
// browse tree, current directory, and selection.
package cli

import (
	"fmt"
	"strings"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/recovery"
)

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
	tty   bool // stdin is a real terminal, so prompting for missing input is safe
}

// runRecoverShell drives the interactive recovery prompt. It reuses the shared
// stdinReader so it coexists with operator swap prompts during extraction.
func runRecoverShell(eng *engine.Engine, dleName, dateStr, timeStr, dest string) error {
	asOf, err := recoverAsOf(dateStr, timeStr)
	if err != nil {
		return err
	}
	sh := &recoverShell{eng: eng, date: asOf, dest: dest, tty: stdinIsTerminal()}
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
	for {
		if sh.tty { // suppress the prompt when stdin is piped (avoids interleaved echo)
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
		if !sh.tty {
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
		// selection, and browse position intact — like a bad-format date. Restate
		// what was kept so "nothing changed" is explicit, not inferred.
		fmt.Printf("note: %v%s\n", err, sh.keptSuffix())
		return
	}
	sh.banner()
}

// keptSuffix names the as-of state a failed rebrowse left in place ("— keeping
// <date> (<run>)"), so a setdate/settime to a backup-less date says what it kept
// instead of silently staying put.
func (sh *recoverShell) keptSuffix() string {
	if sh.sess == nil {
		return ""
	}
	return fmt.Sprintf(" — keeping %s (%s)", sh.date, sh.sess.Tree().TargetRun)
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
		fmt.Printf("note: %v%s\n", err, sh.keptSuffix())
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
		// Only a real terminal may be prompted: with piped/scripted input the reply
		// would swallow the next command (e.g. a literal "quit" becomes ./quit).
		if !sh.tty {
			fmt.Println("error: no destination set (use 'dest <dir>' or 'extract <dir>')")
			return
		}
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
