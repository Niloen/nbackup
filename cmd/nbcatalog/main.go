// Command nbcatalog maintains the local slot-index cache (e.g. rebuild).
package main

import (
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.CmdCatalog(os.Args[1:]); err != nil {
		cli.Fatalf("%v", err)
	}
}
