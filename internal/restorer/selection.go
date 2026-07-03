package restorer

import (
	"fmt"
	"io"
	"strings"

	"github.com/Niloen/nbackup/internal/archivefs"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// OpenRecover builds a browsable filesystem of a DLE as of a date (YYYY-MM-DD) —
// the recover entry point. Member lists are loaded lazily via the store (cache,
// or the on-medium index on a miss), so a fully-cached browse touches no media
// until extract.
func (r *Restorer) OpenRecover(dle, asOf string) (*recovery.Tree, error) {
	return recovery.BuildTree(r.deps.Archives(), dle, asOf, func(runID string, level int) ([]string, error) {
		return r.deps.Store.Members(record.Ref{Run: runID, DLE: dle, Level: level})
	})
}

// ExtractSelection extracts a selected set of files, grouped by their source
// archive, into destDir. It returns the number of member entries extracted.
// Selected-file recovery extracts in plain mode (never deletes) and always
// decodes server-side — a client-only key is infeasible here, so it fails fast
// (browse stays keyless; only extraction needs the key).
func (r *Restorer) ExtractSelection(steps []recovery.ExtractStep, destDir string, log Logf) (int, error) {
	for _, st := range steps {
		if ec, ok := r.deps.EncryptionFor(st.DLE); ok {
			if hardErr, _ := clientSideKeyRestore(ec, st.DLE); hardErr != nil {
				return 0, hardErr
			}
		}
	}
	// Open the selected archives as one ordered, one-pass read (consecutive
	// same-volume reads reuse the mount), then extract each.
	stepByRef := make(map[record.Ref]recovery.ExtractStep, len(steps))
	refs := make([]record.Ref, 0, len(steps))
	for _, st := range steps {
		ref := record.Ref{Run: st.RunID, DLE: st.DLE, Level: st.Level}
		stepByRef[ref] = st
		refs = append(refs, ref)
	}

	d := dest{exec: r.deps.Exec(""), dir: destDir}
	files := 0
	missing, err := r.deps.Store.ReadArchives(refs, "", func(ref record.Ref, open func() (io.ReadCloser, error)) error {
		st := stepByRef[ref]
		// An archive in the chain that holds none of the selected files contributes
		// nothing — skip it silently rather than logging a noisy "extracting 0 file(s)".
		if countFiles(st.Members) == 0 {
			return nil
		}
		log.Log("extracting %d file(s) from %s %s L%d", countFiles(st.Members), st.RunID, r.deps.DisplayDLE(st.DLE), st.Level)
		rc, serr := open()
		if serr != nil {
			return serr
		}
		// Resolve the per-dumptype encrypt block so a per-dumptype passphrase_file is
		// honored on file-level recovery (server-side decode), not just the config-wide one.
		ec, _ := r.deps.EncryptionFor(st.DLE)
		plan := r.planDecode(st.Compress, st.Encrypt, ec, "")
		if err := DecryptHint(st.Encrypt, r.dec.restoreArchive(rc, plan, st.Archiver, d, st.Members)); err != nil {
			return err
		}
		files += countFiles(st.Members)
		return nil
	})
	if err != nil {
		return files, fmt.Errorf("recover: %w", err)
	}
	if len(missing) > 0 {
		return files, fmt.Errorf("recover: %w — one or more selected archives have no available copy", archivefs.ErrMissingCopy)
	}
	return files, nil
}

// countFiles counts the file members in a selection, excluding the parent
// directories the extractor recreates to hold them (the archiver-neutral member
// convention marks directories with a trailing slash). So recovering one nested
// file reports 1, not "2 entries" once its parent dir is counted.
func countFiles(members []string) int {
	n := 0
	for _, m := range members {
		if !strings.HasSuffix(m, "/") {
			n++
		}
	}
	return n
}
