// Command nbdump executes an NBackup run, producing one sealed slot.
package main

import (
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.CmdDump(os.Args[1:]); err != nil {
		cli.Fatalf("%v", err)
	}
}
