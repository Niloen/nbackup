package restorer

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/archiveio"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// OpenRecover builds a browsable filesystem of a DLE as of a date (YYYY-MM-DD) —
// the recover entry point. Member lists are loaded lazily via the store (cache,
// or the on-medium index on a miss), so a fully-cached browse touches no media
// until extract.
func (r *Restorer) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return recovery.BuildTree(r.deps.Archives(), dle, asOf, func(runID string, level int) ([]record.Member, error) {
		return r.deps.Store.Members(archiveio.Ref{Run: runID, DLE: dle, Level: level})
	})
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
func (r *Restorer) ExtractSelection(steps []recovery.ExtractStep, destDir string, log Logf) (int, int, error) {
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
		log.Log("extracting %d file(s) from %s %s L%d", countFilePaths(st.Members), st.RunID, r.deps.DisplayDLE(st.DLE), st.Level)
		// Ranged path first: a framed (or identity-pipeline) archive on a range-capable
		// copy reads only the covering frames of the selected members; an atomic one
		// fetches only the covering atoms. Any missing ingredient falls through to the
		// whole-stream path below.
		handled, rerr := false, error(nil)
		if st.Shape == record.ShapeAtomic {
			handled, rerr = r.extractAtomic(st, d, log)
		} else {
			handled, rerr = r.extractRanged(st, d, log)
		}
		if handled {
			if rerr != nil {
				return rerr
			}
			files += countFilePaths(st.Members)
			return nil
		}
		rc, serr := open()
		if serr != nil {
			return serr
		}
		plan := r.planDecode(st.DLE, st.Compress, st.Encrypt, "")
		var xerr error
		if st.Shape == record.ShapeAtomic {
			sizes, _, aerr := r.atomSizes(archiveio.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level})
			if aerr != nil {
				rc.Close()
				return aerr
			}
			xerr = r.dec.restoreAtomic(rc, plan, st.Archiver, d, st.Members, sizes)
		} else {
			xerr = r.dec.restoreArchive(rc, plan, st.Archiver, d, st.Members)
		}
		if err := DecryptHint(st.Encrypt, xerr); err != nil {
			return err
		}
		files += countFilePaths(st.Members)
		return nil
	})
	if err != nil {
		return files, archives, fmt.Errorf("recover: %w", err)
	}
	if len(missing) > 0 {
		return files, archives, fmt.Errorf("recover: %w — one or more selected archives have no available copy", archivefs.ErrMissingCopy)
	}
	return files, archives, nil
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
