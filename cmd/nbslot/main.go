// Command nbslot lists, inspects, and prunes slots in an NBackup catalog.
package main

import (
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.CmdSlot(os.Args[1:]); err != nil {
		cli.Fatalf("%v", err)
	}
}
