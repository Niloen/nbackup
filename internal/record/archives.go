package record

import "sort"

// ArchivesOf returns the archives of dle, in run order (by run id). A run dumps
// each DLE once, so there is one archive per run.
func ArchivesOf(archives []Archive, dle string) []Archive {
	var out []Archive
	for _, a := range archives {
		if a.DLE == dle {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return RunIDLess(out[i].Run, out[j].Run) })
	return out
}
