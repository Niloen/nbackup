// Command nbplan shows what the next NBackup run would do.
package main

import (
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.CmdPlan(os.Args[1:]); err != nil {
		cli.Fatalf("%v", err)
	}
}
