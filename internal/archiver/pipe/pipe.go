// Package pipe implements archiver.Archiver over user-configured commands — the
// generic escape hatch (Amanda's amraw/script-API analog) for anything that can
// dump itself to stdout and restore itself from stdin: sqlite `.backup`, an LVM
// snapshot, a bespoke application export. The operator supplies the producer and
// consumer as shell commands; NBackup contributes everything around the stream
// (compression, encryption, placement, catalog, verify, drills).
//
// It is deliberately the minimal archiver: full-only (no incremental state, so
// HasBase is always false and the planner schedules fulls), no member list (the
// stream is opaque — CanList is false and structural verify degrades to a clean
// decode drain), no splicing, and an opaque source/destination interpreted only
// by the operator's own commands. Both commands run through the injected
// executor, so a remote DLE's producer runs on the client exactly as tar does.
package pipe

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
)

func init() {
	archiver.Register("pipe", []string{"backup_command", "restore_command", "estimate_command", "extension"}, func(opts archiver.Options, ex programs.Executor, _ string) (archiver.Archiver, error) {
		if ex == nil {
			ex = programs.Local()
		}
		p := &pipe{
			backup:   opts.Get("backup_command"),
			restore:  opts.Get("restore_command"),
			estimate: opts.Get("estimate_command"),
			ext:      opts.Get("extension"),
			ex:       ex,
		}
		if p.backup == "" {
			return nil, fmt.Errorf("pipe archiver: backup_command is required (a shell command writing the backup stream to stdout; {source} substitutes the DLE's source string)")
		}
		if p.restore == "" {
			return nil, fmt.Errorf("pipe archiver: restore_command is required (a shell command consuming the stream on stdin; {dest} substitutes the restore destination)")
		}
		if p.ext == "" {
			p.ext = ".raw"
		} else if !strings.HasPrefix(p.ext, ".") {
			p.ext = "." + p.ext
		}
		return p, nil
	})
}

type pipe struct {
	backup   string // producer: stream to stdout; {source} = the DLE's source string
	restore  string // consumer: stream on stdin; {dest} = the restore destination
	estimate string // optional: prints the estimated byte count; {source} as above
	ext      string // payload extension (normalized to a leading dot); default ".raw"
	ex       programs.Executor
}

func (p *pipe) Name() string { return "pipe" }

// Check verifies a POSIX shell is runnable on the executor's host — the one tool
// pipe itself needs. The configured commands are the operator's own; running them
// to probe them would BE a backup, so readiness of their tools is theirs to keep.
func (p *pipe) Check() error {
	if err := p.ex.Command("sh", "-c", "true").Run(); err != nil {
		return fmt.Errorf("cannot run sh: %w (the pipe archiver runs its commands via `sh -c`)", err)
	}
	return nil
}

// CheckSource: nothing to probe — the source string has meaning only to the
// operator's producer command.
func (p *pipe) CheckSource(string) error { return nil }

// Expand is the identity for pipe: a pipe source is an opaque token for the producer command,
// with no notion of enumerable children, so any wildcard in it is literal. One Scope, no I/O.
// The partition (mapping) form is refused — pipe has no children to match and no remainder.
func (p *pipe) Expand(sp archiver.SourcePattern) ([]archiver.Scope, error) {
	if sp.Base != "" {
		return nil, fmt.Errorf("pipe archiver cannot partition a source (a pipe source has no children); use a plain source")
	}
	return []archiver.Scope{{Source: sp.Pattern, Exclude: sp.Exclude}}, nil
}

// Estimate runs the optional estimate_command (its stdout is the byte count).
// Without one the size is unknown: report 0 rather than guess — the planner
// already treats a zero estimate as "no estimator available".
func (p *pipe) Estimate(r archiver.BackupRequest) (int64, error) {
	if p.estimate == "" {
		return 0, nil
	}
	out, err := p.ex.Command("sh", "-c", substitute(p.estimate, "{source}", r.Source)).Output()
	if err != nil {
		return 0, fmt.Errorf("pipe estimate_command failed: %w", err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("pipe estimate_command must print a byte count, got %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

// BackupSource wraps the producer command as the pipeline's source stage. There is
// no incremental state (Promote is nil), no member index, and no totals side
// channel — the raw size is metered by the caller off the stage's own output.
func (p *pipe) BackupSource(r archiver.BackupRequest) (*archiver.BackupSource, error) {
	if r.Level > 0 {
		return nil, fmt.Errorf("pipe archiver is full-only: it keeps no incremental state, so L%d is not dumpable (it never reports a base, so the planner schedules fulls)", r.Level)
	}
	if len(r.Exclude) > 0 {
		return nil, fmt.Errorf("pipe archiver does not support exclude patterns — drop `exclude` from the dumptype (what the stream contains is the backup_command's own business)")
	}
	stage := programs.Cmd{Name: "sh", Args: []string{"-c", substitute(p.backup, "{source}", r.Source)}}
	finish := func() (*archiver.BackupResult, error) {
		// An opaque stream: no members, no file count, and the archiver cannot
		// measure its own size (the caller's meter on the stage output supplies it).
		return &archiver.BackupResult{}, nil
	}
	return &archiver.BackupSource{Stage: stage, Exec: p.ex, Finish: finish, Cleanup: func() {}}, nil
}

// HasBase: never — pipe keeps no incremental state, so every dump is a full.
func (p *pipe) HasBase(string, int) bool { return false }

// RestoreStage wraps the consumer command: the archive stream arrives on stdin and
// {dest} names the destination exactly as the caller supplied it (`--dest`, or a
// drill's scratch directory). members cannot occur — pipe records none to select.
func (p *pipe) RestoreStage(dest string, _ []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", substitute(p.restore, "{dest}", dest)}}
}

// DestIsDir: the destination is opaque — only the consumer command interprets it,
// so the generic layer must not create, guard, or clear anything there.
func (p *pipe) DestIsDir() bool { return false }

// SourceIsPath: no — the DLE's source is an opaque token the user's backup_command
// interprets ({source}), not necessarily a filesystem path; the generic layer must
// not stat it.
func (p *pipe) SourceIsPath() bool { return false }

func (p *pipe) Ext() string { return p.ext }

// CanList: an opaque stream has no enumerable members; structural verify degrades
// to a clean decode drain.
func (p *pipe) CanList() bool { return false }

func (p *pipe) List(io.Reader) ([]record.Member, error) {
	return nil, fmt.Errorf("pipe archiver cannot list members: the stream is opaque")
}

// StockExtract: the stock recovery IS the operator's own consumer command — with
// the drill's destination riding in as the script's "$1".
func (p *pipe) StockExtract() string {
	return strings.ReplaceAll(p.restore, "{dest}", `"$1"`)
}

// SpliceTrailer: no member extents, nothing to splice.
func (p *pipe) SpliceTrailer() []byte { return nil }

// RestoreIsCombine: no — the consumer command applies each (only) level directly.
func (p *pipe) RestoreIsCombine() bool { return false }

func (p *pipe) CombineStage(string, []string) programs.Cmd { return programs.Cmd{} }

// Assembler: nil — no members, nothing to assemble.
func (p *pipe) Assembler() archiver.Assembler { return nil }

// Exporter: nil — the stream's useful form is whatever the consumer command makes.
func (p *pipe) Exporter() archiver.Exporter { return nil }

// substitute replaces a {placeholder} with its value single-quoted for `sh -c`,
// so a source/dest containing spaces or metacharacters rides as one word and is
// never re-interpreted by the shell.
func substitute(command, placeholder, value string) string {
	quoted := "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	return strings.ReplaceAll(command, placeholder, quoted)
}
