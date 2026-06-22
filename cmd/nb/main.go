// Command nb is the umbrella NBackup CLI dispatching to the nb* subcommands.
package main

import (
	"fmt"
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

const usage = `NBackup - immutable, slot-based backups

Usage: nb <command> [options]

Commands:
  plan       Show what the next run would do (alias: nbplan)
  dump       Execute a run and seal a slot   (alias: nbdump)
  slot       List/show/prune slots           (alias: nbslot)
  verify     Verify slot checksums           (alias: nbverify)
  restore    Restore a DLE from a slot       (alias: nbrestore)
  copy       Copy a slot to another medium   (e.g. disk -> tape)
  catalog    Maintain the local slot cache   (alias: nbcatalog)

Run "nb <command> -h" for command options.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "plan":
		err = cli.CmdPlan(args)
	case "dump":
		err = cli.CmdDump(args)
	case "slot":
		err = cli.CmdSlot(args)
	case "verify":
		err = cli.CmdVerify(args)
	case "restore":
		err = cli.CmdRestore(args)
	case "copy":
		err = cli.CmdCopy(args)
	case "catalog":
		err = cli.CmdCatalog(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		cli.Fatalf("%v", err)
	}
}
