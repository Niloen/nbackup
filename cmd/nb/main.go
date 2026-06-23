// Command nb is the NBackup CLI: a single binary with subcommands.
package main

import (
	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		cli.Fatalf("%v", err)
	}
}
