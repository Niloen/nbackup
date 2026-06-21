// Command nbrestore reconstructs a DLE's data from a slot.
package main

import (
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.CmdRestore(os.Args[1:]); err != nil {
		cli.Fatalf("%v", err)
	}
}
