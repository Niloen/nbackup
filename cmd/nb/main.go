// Command nb is the NBackup CLI: a single binary with subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	err := cli.Execute()
	if err == nil {
		return
	}
	// An operator-initiated cancel (Ctrl-C) is not a failure: report it plainly on its
	// own line — no "error:" prefix — and exit 130 (128+SIGINT), the conventional code
	// a shell uses for a SIGINT-terminated process.
	if errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(130)
	}
	cli.Fatalf("%v", err)
}
