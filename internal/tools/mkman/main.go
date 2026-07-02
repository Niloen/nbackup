// Command mkman generates the nb man pages from the real cobra command tree, so
// the pages can never drift from the CLI's own help text. It writes one section-1
// page per command (nb.1, nb-dump.1, …) into the directory given as its argument
// (default dist/man — untracked; `make man` invokes this, and the release workflow
// runs it before GoReleaser packs the pages into archives and deb/rpm packages):
//
//	go run ./internal/tools/mkman [outdir]
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra/doc"

	"github.com/Niloen/nbackup/internal/cli"
)

func main() {
	outDir := "dist/man"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := run(outDir); err != nil {
		fmt.Fprintln(os.Stderr, "mkman:", err)
		os.Exit(1)
	}
}

func run(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	root := cli.NewRootCmd()
	// GenManTree stamps each page with the generation date by default, which would
	// make the output nondeterministic; pin the header instead. The Source line is
	// what troff shows in the page footer.
	header := &doc.GenManHeader{
		Title:   "NB",
		Section: "1",
		Source:  "NBackup " + cli.Version,
		Manual:  "NBackup Manual",
	}
	if err := doc.GenManTree(root, header, outDir); err != nil {
		return err
	}
	fmt.Printf("wrote man pages to %s\n", outDir)
	return nil
}
