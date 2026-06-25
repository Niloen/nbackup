package transform

import (
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// TestForwardReverseOrder proves the chain runs forward in order and reverse in reverse
// order, and that identity (none) filters drop out of both directions.
func TestForwardReverseOrder(t *testing.T) {
	ex := programs.Local()
	gzip := programs.Filter{Name: "gzip", Forward: programs.Cmd{Name: "gzip"}, Reverse: programs.Cmd{Name: "gunzip"}}
	gpg := programs.Filter{Name: "gpg", Forward: programs.Cmd{Name: "gpg-e"}, Reverse: programs.Cmd{Name: "gpg-d"}}
	none := programs.Filter{Name: "none"} // identity — zero cmds

	p := Pipeline{{Filter: gzip, Exec: ex}, {Filter: none, Exec: ex}, {Filter: gpg, Exec: ex}}

	fwd := p.Forward()
	if got := names(fwd); got != "gzip,gpg-e" {
		t.Errorf("Forward = %q, want compress then encrypt (none skipped)", got)
	}
	rev := p.Reverse()
	if got := names(rev); got != "gpg-d,gunzip" {
		t.Errorf("Reverse = %q, want decrypt then decompress (none skipped)", got)
	}
}

func names(stages []programs.Stage) string {
	out := ""
	for i, s := range stages {
		if i > 0 {
			out += ","
		}
		out += s.Cmd.Name
	}
	return out
}
