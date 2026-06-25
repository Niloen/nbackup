package transform

import (
	"testing"

	"github.com/Niloen/nbackup/internal/hostexec"
)

// TestForwardReverseOrder proves the chain runs forward in order and reverse in reverse
// order, and that identity (none) filters drop out of both directions.
func TestForwardReverseOrder(t *testing.T) {
	ex := hostexec.Local()
	gzip := hostexec.Filter{Name: "gzip", Forward: hostexec.Cmd{Name: "gzip"}, Reverse: hostexec.Cmd{Name: "gunzip"}}
	gpg := hostexec.Filter{Name: "gpg", Forward: hostexec.Cmd{Name: "gpg-e"}, Reverse: hostexec.Cmd{Name: "gpg-d"}}
	none := hostexec.Filter{Name: "none"} // identity — zero cmds

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

func names(stages []hostexec.Stage) string {
	out := ""
	for i, s := range stages {
		if i > 0 {
			out += ","
		}
		out += s.Cmd.Name
	}
	return out
}
