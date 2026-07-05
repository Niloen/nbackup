package restorer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// This file is unit export: materializing named inventory units (a table, as
// of a date) into their directly-useful form (postgres: pg_dump SQL). From a
// physical backup that inherently means restoring the WHOLE DLE first — the
// honest cost, stated to the caller before it starts — into scratch, then
// running the archiver's Exporter there and removing the scratch. The output
// is a file the operator imports themselves; no live service is ever touched.

// ExportUnits materializes the named units of a DLE as of a date into destDir,
// each landing as "<unit><ext>" (the exporter's extension). Names resolve
// against the inventory with the standard pointing rules (exact, then unique
// substring); a miss or an ambiguity is an error naming the candidates. It
// returns the files written.
func (r *Restorer) ExportUnits(dle, asOf string, names []string, destDir string, log Logf) ([]string, error) {
	units, _, err := r.Inventory(dle, asOf)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("%s records no inventory — its archiver reports no named units to export", r.deps.DisplayDLE(dle))
	}
	resolved, err := resolveUnits(units, names)
	if err != nil {
		return nil, err
	}

	// The chain's archiver must offer an exporter (one chain, one type — the
	// first step answers, as everywhere on the read side).
	target, err := recovery.AsOf(r.deps.Archives(), asOf)
	if err != nil {
		return nil, err
	}
	steps, err := recovery.Chain(r.deps.Archives(), dle, target)
	if err != nil {
		return nil, r.friendlyDLEErr(dle, err)
	}
	arch, err := r.deps.ArchiverFor(steps[0].Archiver, dle, "")
	if err != nil {
		return nil, err
	}
	exp := arch.Exporter()
	if exp == nil {
		return nil, fmt.Errorf("archiver %q has no export capability — select the unit's files with --path instead", steps[0].Archiver)
	}

	// The honest cost, said out loud: a physical backup only yields a table
	// through the whole restored DLE.
	var total int64
	for _, st := range steps {
		for _, a := range r.deps.Archives() {
			if a.Run == st.RunID && a.DLE == st.DLE && a.Level == st.Level {
				total += a.Uncompressed
			}
		}
	}
	log.Log("exporting %d unit(s) restores the whole DLE (~%s) to scratch first", len(resolved), readSize(total))

	scratch, err := os.MkdirTemp("", "nbackup-export-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	dataDir := filepath.Join(scratch, "data")
	if err := r.Extract(Request{DLE: dle, RunID: target, Dest: dataDir}, log); err != nil {
		return nil, fmt.Errorf("export: scratch restore: %w", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	unitNames := make([]string, len(resolved))
	for i, u := range resolved {
		unitNames[i] = u.Path
	}
	log.Log("materializing %s", strings.Join(unitNames, ", "))
	source := ""
	if r.deps.SourceOf != nil {
		source = r.deps.SourceOf(dle)
	}
	if err := runStage(r.deps.Exec(""), exp.Stage(dataDir, destDir, source, unitNames)); err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	written := make([]string, len(unitNames))
	for i, n := range unitNames {
		written[i] = filepath.Join(destDir, n+exp.Ext())
	}
	return written, nil
}

// resolveUnits maps each pointed-at name to exactly one unit, deduplicating
// (two arguments may match the same unit) and keeping a deterministic order.
func resolveUnits(units []record.Unit, names []string) ([]record.Unit, error) {
	chosen := map[string]record.Unit{}
	for _, name := range names {
		matches := recovery.MatchUnits(units, name)
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no unit matches %q (see --inventory for the recorded names)", name)
		case 1:
			chosen[matches[0].Path] = matches[0]
		default:
			var cands []string
			for _, m := range matches {
				cands = append(cands, m.Path)
			}
			return nil, fmt.Errorf("%q matches %d units — use a fuller name: %s", name, len(matches), strings.Join(cands, ", "))
		}
	}
	out := make([]record.Unit, 0, len(chosen))
	for _, u := range chosen {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
