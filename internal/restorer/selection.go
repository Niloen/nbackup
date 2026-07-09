package restorer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
	"github.com/Niloen/nbackup/internal/sizeutil"
)

// readSize renders a read/egress figure for the recovery log, degrading to a neutral
// phrase when the size is unknown (no catalog metadata) rather than printing "0 B".
func readSize(n int64) string {
	if n <= 0 {
		return "unknown-size"
	}
	return sizeutil.FormatBytes(n)
}

// OpenRecover builds a browsable filesystem of a DLE as of a date (YYYY-MM-DD) —
// the recover entry point. Member lists are loaded lazily via the store (cache,
// or the on-medium index on a miss), so a fully-cached browse touches no media
// until extract.
func (r *Restorer) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return recovery.BuildTree(r.deps.Archives(), dle, asOf, r.membersFor(dle), r.assemblerFor(dle))
}

// OpenRecoverRun is OpenRecover pinned to an exact run — the mount's per-run
// snapshot view; see recovery.BuildTreeForRun.
func (r *Restorer) OpenRecoverRun(dle, runID string) (*recovery.Tree, error) {
	return recovery.BuildTreeForRun(r.deps.Archives(), dle, runID, r.membersFor(dle), r.assemblerFor(dle))
}

// membersFor is the lazy member-list loader BuildTree wants: cache first, the
// on-medium index on a miss (see archivefs.ReadStore.Members).
func (r *Restorer) membersFor(dle string) func(runID string, level int) ([]record.Member, error) {
	return func(runID string, level int) ([]record.Member, error) {
		return r.deps.Store.Members(archiveio.Ref{Run: runID, DLE: dle, Level: level})
	}
}

// Inventory returns a DLE's content inventory as of a date: the units of the
// NEWEST chain archive that reports any (each dump's inventory is a complete
// statement about content, so the tip speaks for the chain — the same instinct
// as the assembler census), plus the run that reported it. Nil units = the
// archiver records no inventory (gnutar, pipe) — the caller says so rather
// than showing an empty table.
//
// The RECORDED unit members are raw archive tokens (an incremental's delta
// files, "base/5/INCREMENTAL.2619"); the SERVED inventory normalizes them to
// their logical browse paths via the archiver's Assembler, so what the user
// sees is selectable in the browse tree as-is (`add`, `--path`).
func (r *Restorer) Inventory(dle, asOf string) ([]record.Unit, string, error) {
	target, err := recovery.AsOf(r.deps.Archives(), asOf)
	if err != nil {
		return nil, "", err
	}
	steps, err := recovery.Chain(r.deps.Archives(), dle, target)
	if err != nil {
		return nil, "", r.friendlyDLEErr(dle, err)
	}
	for i := len(steps) - 1; i >= 0; i-- {
		idx, err := r.deps.Store.Index(archiveio.Ref{Run: steps[i].RunID, DLE: dle, Level: steps[i].Level})
		if err != nil {
			return nil, "", err
		}
		if len(idx.Units) > 0 {
			if asm := r.assemblerFor(dle)(steps[i].Archiver); asm != nil {
				for u := range idx.Units {
					for m, member := range idx.Units[u].Members {
						idx.Units[u].Members[m], _ = asm.Logical(member)
					}
				}
			}
			return idx.Units, steps[i].RunID, nil
		}
	}
	return nil, target, nil
}

// assemblerFor resolves the browse-time chain assembler for a recorded archiver
// type, through the DLE's config as every read-side resolution does. An
// unresolvable archiver yields nil — the tree then keeps its default
// most-recent-wins view, and extraction of a delta member surfaces the real
// resolution error.
func (r *Restorer) assemblerFor(dle string) recovery.AssemblerFor {
	return func(archiverType string) archiver.Assembler {
		arch, err := r.deps.ArchiverFor(archiverType, "", dle, "")
		if err != nil {
			return nil
		}
		return arch.Assembler()
	}
}

// ExtractSelection extracts a selected set of files, grouped by their source
// archive, into destDir. It returns the number of member entries extracted.
// Selected-file recovery extracts in plain mode (never deletes) and always
// decodes server-side — a client-only key is infeasible here, so it fails fast
// (browse stays keyless; only extraction needs the key).
// ExtractSelection extracts the selected files and returns how many files were
// recovered and, second, from how many distinct archives — the archive count reflects
// only the archives actually read (a chain step holding none of the selection is
// skipped, so it is not counted), matching the "extracting …" lines it logs.
// Assemblies are the delta-tipped files no single archive holds (see
// recovery.Assembly): their chain versions are fetched and merged by the
// archiver's Assembler after the plain extractions.
func (r *Restorer) ExtractSelection(steps []recovery.ExtractStep, asms []recovery.Assembly, destDir string, log Logf, prog ReadProgress) (int, int, error) {
	if prog == nil {
		prog = noProgress{}
	}
	for _, st := range steps {
		if ec, ok := r.deps.EncryptionFor(st.DLE); ok {
			if hardErr, _ := clientSideKeyRestore(ec, st.DLE); hardErr != nil {
				return 0, 0, hardErr
			}
		}
	}
	// Open the selected archives as one ordered, one-pass read (consecutive
	// same-volume reads reuse the mount), then extract each.
	stepByRef := make(map[archiveio.Ref]recovery.ExtractStep, len(steps))
	refs := make([]archiveio.Ref, 0, len(steps))
	for _, st := range steps {
		ref := archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
		stepByRef[ref] = st
		refs = append(refs, ref)
	}

	d := dest{exec: r.deps.Exec(""), dir: destDir}
	// Create the destination up front, before any decode work, so an unwritable --dest
	// fails cleanly as a setup error rather than after logging "extracting …" and
	// appearing to have started (the whole-DLE Extract path validates it the same way).
	if err := d.exec.MkdirAll(d.dir); err != nil {
		return 0, 0, errors.Join(errDestSetup, err)
	}
	files := 0
	archives := 0
	missing, err := r.deps.Store.OpenArchives(refs, "", func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		st := stepByRef[ref]
		// An archive in the chain that holds none of the selected files contributes
		// nothing — skip it silently rather than logging a noisy "extracting 0 file(s)".
		if countFilePaths(st.Members) == 0 {
			return nil
		}
		archives++
		nfiles := countFilePaths(st.Members)
		label := fmt.Sprintf("%s %s L%d", st.RunID, r.deps.DisplayDLE(st.DLE), st.Level)
		log.Log("extracting %d file(s) from %s", nfiles, label)
		whole := r.encodedSize(archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level})
		// Ranged path first: on a range-capable copy only the covering frames (or
		// atoms) of the selected members are read. Any missing ingredient falls
		// through to the whole-stream path below.
		if handled, egress, rerr := r.extractSelected(st, d, prog, label); handled {
			prog.Finished()
			if rerr != nil {
				return rerr
			}
			log.Log("  ranged read: fetched %s of the %s archive (only the selected file(s))", readSize(egress), readSize(whole))
			files += nfiles
			return nil
		}
		log.Log("  reading the whole %s archive (its bytes can't be fetched in ranges)", readSize(whole))
		rc, serr := open()
		if serr != nil {
			return serr
		}
		prog.Reading("WHOLE", label, whole)
		rc = &countReadCloser{ReadCloser: rc, prog: prog}
		plan, perr := r.planDecode(st.Step, "")
		if perr != nil {
			rc.Close()
			prog.Finished()
			return perr
		}
		if err := DecryptHint(st.Encrypt, r.dec.restoreArchive(rc, plan, st.Archiver, st.ArchiverName, st.DLE, d, st.Members)); err != nil {
			prog.Finished()
			return err
		}
		prog.Finished()
		files += countFilePaths(st.Members)
		return nil
	})
	if err != nil {
		return files, archives, fmt.Errorf("recover: %w", err)
	}
	if len(missing) > 0 {
		return files, archives, fmt.Errorf("recover: %w — one or more selected archives have no available copy", archivefs.ErrMissingCopy)
	}
	if len(asms) > 0 {
		n, na, aerr := r.extractAssemblies(asms, d, log)
		files += n
		archives += na
		if aerr != nil {
			return files, archives, fmt.Errorf("recover: %w", aerr)
		}
	}
	return files, archives, nil
}

// extractAssemblies lands the delta-tipped files: every chain version's raw
// member is extracted into per-archive scratch (one ordered media pass), then
// each file is merged by the archiver's Assembler and written at its logical
// path — how a table file is recovered from an incremental chain without
// rebuilding the whole cluster.
func (r *Restorer) extractAssemblies(asms []recovery.Assembly, d dest, log Logf) (int, int, error) {
	first := asms[0].Versions[0].Src
	arch, err := r.deps.ArchiverFor(first.Archiver, first.ArchiverName, first.DLE, "")
	if err != nil {
		return 0, 0, err
	}
	asm := arch.Assembler()
	if asm == nil {
		return 0, 0, fmt.Errorf("archiver %q reports no assembler, but the selection needs %d file(s) assembled from incremental deltas", first.Archiver, len(asms))
	}

	// Group the needed members per archive, in chain (run) order.
	type fetch struct {
		step    recovery.Step
		members []string
	}
	fetches := map[archiveio.Ref]*fetch{}
	var refs []archiveio.Ref
	for _, a := range asms {
		for _, v := range a.Versions {
			ref := archiveio.Ref{Run: v.Src.RunID, DLE: v.Src.DLE, Level: v.Src.Level}
			f, ok := fetches[ref]
			if !ok {
				f = &fetch{step: v.Src.Step}
				fetches[ref] = f
				refs = append(refs, ref)
			}
			f.members = append(f.members, v.Src.Member)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Run != refs[j].Run {
			return refs[i].Run < refs[j].Run
		}
		return refs[i].Level < refs[j].Level
	})

	scratch, err := os.MkdirTemp("", "nbackup-assemble-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(scratch)
	// Per-archive scratch subdirectories: two levels' deltas of one file share
	// a member name (INCREMENTAL.<x> in each), so they must land apart.
	refDir := func(ref archiveio.Ref) string {
		return filepath.Join(scratch, fmt.Sprintf("%s-L%d", ref.Run, ref.Level))
	}
	archives := 0
	missing, err := r.deps.Store.OpenArchives(refs, "", func(ref archiveio.Ref, open func() (io.ReadCloser, error)) error {
		f := fetches[ref]
		archives++
		log.Log("reading %d version(s) from %s %s L%d", len(f.members), f.step.RunID, r.deps.DisplayDLE(f.step.DLE), f.step.Level)
		rc, serr := open()
		if serr != nil {
			return serr
		}
		plan, perr := r.planDecode(f.step, "")
		if perr != nil {
			rc.Close()
			return perr
		}
		return DecryptHint(f.step.Encrypt, r.dec.restoreArchive(rc, plan, f.step.Archiver, f.step.ArchiverName, f.step.DLE, dest{exec: d.exec, dir: refDir(ref)}, f.members))
	})
	if err != nil {
		return 0, archives, err
	}
	if len(missing) > 0 {
		return 0, archives, fmt.Errorf("%w — a chain version of an assembled file has no available copy", archivefs.ErrMissingCopy)
	}

	files := 0
	for _, a := range asms {
		if err := r.assembleOne(asm, a, refDir, d.dir); err != nil {
			return files, archives, err
		}
		files++
	}
	log.Log("assembled %d file(s) from their incremental chain", files)
	return files, archives, nil
}

// assembleOne merges one file's fetched versions and writes it at its logical
// path under destDir.
func (r *Restorer) assembleOne(asm archiver.Assembler, a recovery.Assembly, refDir func(archiveio.Ref) string, destDir string) error {
	versions := make([]archiver.Version, 0, len(a.Versions))
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, v := range a.Versions {
		ref := archiveio.Ref{Run: v.Src.RunID, DLE: v.Src.DLE, Level: v.Src.Level}
		f, err := os.Open(filepath.Join(refDir(ref), filepath.FromSlash(v.Src.Member)))
		if err != nil {
			return fmt.Errorf("assemble %s: %w", a.Path, err)
		}
		closers = append(closers, f)
		versions = append(versions, archiver.Version{R: f, Delta: v.Delta})
	}
	rc, err := asm.Assemble(versions)
	if err != nil {
		return fmt.Errorf("assemble %s: %w", a.Path, err)
	}
	defer rc.Close()
	out := filepath.Join(destDir, filepath.FromSlash(a.Path))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rc); err != nil {
		w.Close()
		return fmt.Errorf("assemble %s: %w", a.Path, err)
	}
	return w.Close()
}

// countFilePaths counts the file entries in a raw member-path list (an ExtractStep's
// selection), excluding directories per the trailing-slash convention — the path-only
// twin of archiver.CountFiles.
func countFilePaths(paths []string) int {
	n := 0
	for _, p := range paths {
		if !strings.HasSuffix(p, "/") {
			n++
		}
	}
	return n
}
