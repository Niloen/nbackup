package restorer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// combineArchiver is a combine-shaped fake (postgres's restore model): each
// level's RestoreStage writes a marker into its staging dir, and CombineStage
// records what it was handed by writing a manifest into dest and removing the
// staging — asserting the restorer's gather-then-combine contract without
// pg_combinebackup.
type combineArchiver struct{ fakeArchiver }

func (combineArchiver) RestoreStage(destDir string, members []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c",
		fmt.Sprintf("cat > %s/level-payload", destDir)}}
}

func (combineArchiver) RestoreIsCombine() bool { return true }

func (combineArchiver) CombineStage(dest string, stagingDirs []string) programs.Cmd {
	script := fmt.Sprintf("echo %s > %s/combined.txt && rm -rf %s/.nb-combine",
		strings.Join(stagingDirs, " "), dest, dest)
	return programs.Cmd{Name: "sh", Args: []string{"-c", script}}
}

// TestExtractChainCombine: a combine-shaped chain stages every level into its
// own directory under dest/.nb-combine (level payloads land apart, nothing in
// dest itself), then CombineStage runs exactly once with the staging dirs in
// chain order.
func TestExtractChainCombine(t *testing.T) {
	dle := "db01"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("full"),
		ref("run-2026-06-02.001", dle, 1): []byte("incr"),
	}}
	deps := testDeps(store, chainArchives(dle))
	deps.ArchiverFor = func(typeName, dle, host string) (archiver.Archiver, error) {
		return combineArchiver{}, nil
	}
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	if err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: dest}, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "combined.txt"))
	if err != nil {
		t.Fatalf("combine never ran: %v", err)
	}
	wantL0 := filepath.Join(dest, ".nb-combine", "L0")
	wantL1 := filepath.Join(dest, ".nb-combine", "L1")
	if want := wantL0 + " " + wantL1; strings.TrimSpace(string(got)) != want {
		t.Fatalf("combine got %q, want %q (staging dirs in chain order)", strings.TrimSpace(string(got)), want)
	}
	// CombineStage owned the staging teardown; each level extracted into its
	// own dir (the payloads never touched dest directly).
	if _, err := os.Stat(filepath.Join(dest, ".nb-combine")); !os.IsNotExist(err) {
		t.Fatal("staging survived the combine")
	}
	if _, err := os.Stat(filepath.Join(dest, "level-payload")); !os.IsNotExist(err) {
		t.Fatal("a level extracted straight into dest — combine chains must stage")
	}
}

// TestExtractChainCombineFailureRollsBack: a failing combine clears the
// destination (staging included) exactly like a failing additive chain — no
// half-restored tree survives.
func TestExtractChainCombineFailureRollsBack(t *testing.T) {
	dle := "db01"
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		ref("run-2026-06-01.001", dle, 0): []byte("full"),
		ref("run-2026-06-02.001", dle, 1): []byte("incr"),
	}}
	deps := testDeps(store, chainArchives(dle))
	deps.ArchiverFor = func(typeName, dle, host string) (archiver.Archiver, error) {
		return failingCombine{}, nil
	}
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	if err := r.Extract(Request{DLE: dle, RunID: "run-2026-06-02.001", Dest: dest}, nil); err == nil {
		t.Fatal("want combine failure")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dest not rolled back; holds %v", entries)
	}
}

type failingCombine struct{ combineArchiver }

func (failingCombine) CombineStage(dest string, stagingDirs []string) programs.Cmd {
	return programs.Cmd{Name: "sh", Args: []string{"-c", "exit 1"}}
}

// TestAssembledSelection: a delta-tipped selection is fetched version by
// version and merged by the archiver's Assembler at its logical path — driven
// through ExtractSelection with real (tar-free) payload plumbing faked at the
// archiver seam.
func TestAssembledSelection(t *testing.T) {
	dle := "db01"
	refs := []archiveio.Ref{ref("run-2026-06-01.001", dle, 0), ref("run-2026-06-02.001", dle, 1)}
	store := &fakeStore{payloads: map[archiveio.Ref][]byte{
		refs[0]: []byte("base-bytes"),
		refs[1]: []byte("delta-bytes"),
	}}
	deps := testDeps(store, chainArchives(dle))
	deps.ArchiverFor = func(typeName, dle, host string) (archiver.Archiver, error) {
		return assemblingArchiver{}, nil
	}
	r := New(deps)

	asm := []recovery.Assembly{{
		Path: "base/5/2619",
		Versions: []recovery.Version{
			{Src: &recovery.Source{Step: recovery.Step{RunID: refs[0].Run, DLE: dle, Level: 0, Archiver: "postgres", Compress: "none"}, Member: "base/5/2619"}},
			{Src: &recovery.Source{Step: recovery.Step{RunID: refs[1].Run, DLE: dle, Level: 1, Archiver: "postgres", Compress: "none"}, Member: "base/5/INCREMENTAL.2619"}, Delta: true},
		},
	}}
	out := filepath.Join(t.TempDir(), "sel")
	files, archives, err := r.ExtractSelection(nil, asm, out, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || archives != 2 {
		t.Fatalf("files=%d archives=%d", files, archives)
	}
	got, err := os.ReadFile(filepath.Join(out, "base/5/2619"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake assembler concatenates "<whole>|<delta>" — proving both
	// versions arrived, in order, with the delta flag intact.
	if string(got) != "whole:full|delta:incr" {
		t.Fatalf("assembled = %q", got)
	}
}

// assemblingArchiver stages each requested member as a file whose content is
// the archive payload, and its Assembler concatenates the versions with their
// delta-ness — enough to prove the plumbing end to end.
type assemblingArchiver struct{ fakeArchiver }

func (assemblingArchiver) RestoreStage(destDir string, members []string) programs.Cmd {
	// Write the stage's stdin (the archive payload) to every requested member path.
	var sh []string
	for _, m := range members {
		sh = append(sh, fmt.Sprintf("mkdir -p %s && tee %s > /dev/null",
			filepath.Dir(filepath.Join(destDir, m)), filepath.Join(destDir, m)))
	}
	return programs.Cmd{Name: "sh", Args: []string{"-c", strings.Join(sh, " && ")}}
}

func (assemblingArchiver) Assembler() archiver.Assembler { return concatAssembler{} }

type concatAssembler struct{}

func (concatAssembler) Logical(p string) (string, bool) { return p, false }

func (concatAssembler) Assemble(versions []archiver.Version) (io.ReadCloser, error) {
	var parts []string
	for _, v := range versions {
		b, err := io.ReadAll(v.R)
		if err != nil {
			return nil, err
		}
		kind := "whole"
		if v.Delta {
			kind = "delta"
		}
		// The fake payloads are "full"/"incr" carried via the fake RestoreStage.
		parts = append(parts, kind+":"+translate(string(b)))
	}
	return io.NopCloser(strings.NewReader(strings.Join(parts, "|"))), nil
}

// translate maps the fake archive payloads to short markers.
func translate(payload string) string {
	switch payload {
	case "base-bytes":
		return "full"
	case "delta-bytes":
		return "incr"
	}
	return payload
}

// exportingArchiver: combine-shaped with an exporter whose stage proves the
// ordering contract — it reads a marker CombineStage left in the restored
// tree, so an export that ran before (or without) the scratch restore fails.
type exportingArchiver struct{ combineArchiver }

func (exportingArchiver) Exporter() archiver.Exporter { return fakeExporter{} }

type fakeExporter struct{}

func (fakeExporter) Ext() string { return ".sql" }

func (fakeExporter) Stage(dataDir, destDir string, units []string) programs.Cmd {
	var sh []string
	for _, u := range units {
		sh = append(sh, fmt.Sprintf("printf 'exported %%s from %%s' %s \"$(cat %s/combined.txt)\" > %s",
			u, dataDir, filepath.Join(destDir, u+".sql")))
	}
	return programs.Cmd{Name: "sh", Args: []string{"-c", strings.Join(sh, " && ")}}
}

// TestExportUnits drives the whole export orchestration over fakes: inventory
// resolution (unique-substring pointing), the scratch whole-DLE restore, the
// exporter stage on the restored tree, and the written-file contract
// (<identity><ext> under dest).
func TestExportUnits(t *testing.T) {
	dle := "db01"
	tip := ref("run-2026-06-02.001", dle, 1)
	store := &fakeStore{
		payloads: map[archiveio.Ref][]byte{
			ref("run-2026-06-01.001", dle, 0): []byte("full"),
			tip:                               []byte("incr"),
		},
		units: map[archiveio.Ref][]record.Unit{
			tip: {{Path: "table.postgres.public.users", Size: 42}},
		},
	}
	deps := testDeps(store, chainArchives(dle))
	deps.ArchiverFor = func(typeName, dle, host string) (archiver.Archiver, error) {
		return exportingArchiver{}, nil
	}
	r := New(deps)
	dest := filepath.Join(t.TempDir(), "out")
	written, err := r.ExportUnits(dle, "2026-06-02", []string{"public.users"}, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0] != filepath.Join(dest, "table.postgres.public.users.sql") {
		t.Fatalf("written = %v", written)
	}
	got, err := os.ReadFile(written[0])
	if err != nil {
		t.Fatal(err)
	}
	// The exporter saw the combined scratch tree (its stage read the combine marker).
	if !strings.HasPrefix(string(got), "exported table.postgres.public.users from ") ||
		!strings.Contains(string(got), ".nb-combine/L0") {
		t.Fatalf("export content = %q", got)
	}

	// Pointing failures are deterministic errors.
	if _, err := r.ExportUnits(dle, "2026-06-02", []string{"nope"}, dest, nil); err == nil || !strings.Contains(err.Error(), "no unit matches") {
		t.Fatalf("miss = %v", err)
	}
	// No exporter → a clear capability error.
	deps.ArchiverFor = func(typeName, dle, host string) (archiver.Archiver, error) {
		return combineArchiver{}, nil
	}
	if _, err := New(deps).ExportUnits(dle, "2026-06-02", []string{"public.users"}, dest, nil); err == nil || !strings.Contains(err.Error(), "no export capability") {
		t.Fatalf("capability error = %v", err)
	}
}
