// recover_line.go is the recover shell's line-editing layer: a golang.org/x/term
// line editor (history, cursor/word editing, tab-completion) when stdin is a real
// terminal, plus the shell-style argument tokenizer both the dispatcher and the
// completer share (so a path with a space can be one argument: add "My Photo.jpg").
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// recoverCommands are the shell's command words, for first-word tab-completion.
var recoverCommands = []string{
	"disks", "setdisk", "setdate", "settime", "ls", "cd", "pwd",
	"add", "delete", "list", "clear", "dest", "extract", "help", "quit",
}

// termRW pairs the shared stdin reader with stdout for x/term. The terminal reads
// through stdinReader (not os.Stdin directly) so it shares one buffer with the
// cooked-mode prompts issued mid-extraction (cost confirm, destination, tape swap):
// bytes the editor read past a line stay in the same buffer those prompts drain.
type termRW struct {
	io.Reader
	io.Writer
}

// setupLineEditor attaches an x/term line editor when stdin is a terminal, enabling
// history, editing, and tab-completion; on a pipe (or if it can't be created) the
// shell falls back to plain buffered reads.
func (sh *recoverShell) setupLineEditor() {
	if !sh.tty {
		return
	}
	sh.fd = int(os.Stdin.Fd())
	sh.term = term.NewTerminal(termRW{stdinReader, os.Stdout}, "")
	sh.term.AutoCompleteCallback = sh.autoComplete
}

// readLine reads one command line. With the terminal editor it flips the tty to raw
// mode only for the duration of the read (so every other line of output — listings,
// notes, the extraction progress bar, the cooked-mode prompts — runs in normal cooked
// mode and needs no \r handling), draws the prompt itself, and returns the edited
// line. Without it, it prints the prompt (on a tty) and reads a cooked line.
func (sh *recoverShell) readLine(prompt string) (string, error) {
	if sh.term != nil {
		old, err := term.MakeRaw(sh.fd)
		if err == nil {
			defer term.Restore(sh.fd, old)
			sh.term.SetPrompt(prompt)
			return sh.term.ReadLine()
		}
	}
	if sh.tty {
		fmt.Print(prompt)
	}
	line, err := stdinReader.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// argToken is one tokenized argument: its unquoted text and the byte offset in the
// line where it starts (the completer replaces the token in place).
type argToken struct {
	text  string
	start int
}

// scanArgs splits a command line into arguments, so the dispatcher sees a path with a
// space as one argument.
func scanArgs(line string) []string {
	toks, _ := scanTokens(line)
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}

// scanTokens tokenizes line shell-style: whitespace separates arguments, single quotes
// quote literally, double quotes quote with backslash escaping, and a backslash escapes
// the next character. atSep reports whether the scan ended on an unquoted separator (or
// an empty line) — i.e. the cursor at end-of-line begins a fresh, empty token — which
// the completer needs to tell "add <tab>" (complete a new path) from "ad<tab>" (still
// completing the command word). An unterminated quote or trailing backslash keeps the
// open token, so completion works mid-quote.
func scanTokens(line string) (toks []argToken, atSep bool) {
	var b strings.Builder
	inTok := false
	start := 0
	esc := false
	var quote byte // 0, '\'', or '"'
	flush := func() {
		if inTok {
			toks = append(toks, argToken{text: b.String(), start: start})
			b.Reset()
			inTok = false
		}
	}
	begin := func(i int) {
		if !inTok {
			inTok, start = true, i
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case esc:
			begin(i - 1)
			b.WriteByte(c)
			esc = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				b.WriteByte(c)
			}
		case c == '\\':
			esc = true
		case c == '\'' || c == '"':
			begin(i)
			quote = c
		case c == ' ' || c == '\t':
			flush()
		default:
			begin(i)
			b.WriteByte(c)
		}
	}
	atSep = !inTok && quote == 0 && !esc
	flush()
	return toks, atSep
}

// autoComplete is the x/term completion callback (invoked per keypress; it acts only on
// Tab). It resolves the token under the cursor and offers command names for the first
// word, browse-tree paths for path commands, and DLE names for setdisk — quoting a
// candidate that contains a space so it round-trips through scanTokens.
func (sh *recoverShell) autoComplete(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}
	toks, atSep := scanTokens(line[:pos])
	curText, curStart := "", pos
	if !atSep && len(toks) > 0 {
		last := toks[len(toks)-1]
		curText, curStart = last.text, last.start
	}
	firstWord := len(toks) == 0 || (len(toks) == 1 && !atSep)

	var cands []string
	if firstWord {
		cands = prefixMatch(recoverCommands, curText)
	} else {
		cands = sh.argCandidates(toks[0].text, curText)
	}
	repl, ok := completion(curText, cands)
	if !ok {
		return "", 0, false
	}
	return line[:curStart] + repl + line[pos:], curStart + len(repl), true
}

// completion turns candidate values into the replacement text for the current token:
// a lone match is completed in full (a trailing "/" keeps the cursor at a directory,
// anything else gets a trailing space to move on), several matches extend to their
// common prefix. A candidate needing quotes is quoted so it round-trips.
func completion(curText string, cands []string) (string, bool) {
	switch len(cands) {
	case 0:
		return "", false
	case 1:
		v := cands[0]
		if strings.HasSuffix(v, "/") {
			return quoteArg(v), true
		}
		return quoteArg(v) + " ", true
	default:
		lcp := commonPrefix(cands)
		if len(lcp) <= len(curText) {
			return "", false // no shared progress past what's typed
		}
		if strings.ContainsAny(lcp, " \t'\"\\") {
			return "\"" + escapeDoubleQuoted(lcp), true // open quote: keep typing inside it
		}
		return lcp, true
	}
}

// argCandidates lists completion values for a command's argument at the given prefix.
func (sh *recoverShell) argCandidates(cmd, prefix string) []string {
	switch cmd {
	case "add", "delete", "rm", "del", "ls", "dir", "cd":
		return sh.pathCandidates(prefix, cmd == "cd")
	case "setdisk", "disk", "sethost":
		return prefixMatch(sh.eng.DLEDisplay(), prefix)
	default:
		return nil
	}
}

// pathCandidates lists browse-tree entries under the directory named in prefix that
// match its final component (only directories when onlyDirs, for cd). Each candidate
// is the whole relative path the user would type, directories suffixed with "/".
func (sh *recoverShell) pathCandidates(prefix string, onlyDirs bool) []string {
	if sh.sess == nil {
		return nil
	}
	dirPart, base := "", prefix
	if k := strings.LastIndex(prefix, "/"); k >= 0 {
		dirPart, base = prefix[:k], prefix[k+1:]
	}
	lookup, join := dirPart, ""
	if dirPart != "" {
		join = dirPart + "/"
	} else if strings.HasPrefix(prefix, "/") {
		lookup, join = "/", "/" // completing directly under the DLE root
	}
	n, ok := sh.sess.Lookup(lookup)
	if !ok || !n.IsDir() {
		return nil
	}
	var out []string
	for _, c := range n.Children() {
		if !strings.HasPrefix(c.Name(), base) || (onlyDirs && !c.IsDir()) {
			continue
		}
		v := join + c.Name()
		if c.IsDir() {
			v += "/"
		}
		out = append(out, v)
	}
	return out
}

// quoteArg renders s so scanTokens reads it back as the same argument: bare when it
// has no shell-special character, single-quoted when it does but holds no single
// quote, else double-quoted with escapes.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t'\"\\") {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return `"` + escapeDoubleQuoted(s) + `"`
}

// escapeDoubleQuoted backslash-escapes the characters that are special inside double
// quotes, for use between double quotes.
func escapeDoubleQuoted(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// prefixMatch returns the items of list that start with prefix, order preserved.
func prefixMatch(list []string, prefix string) []string {
	var out []string
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// commonPrefix returns the longest byte-prefix shared by every string in ss.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}
