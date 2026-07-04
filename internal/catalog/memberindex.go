package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/Niloen/nbackup/internal/fsx"
	"github.com/Niloen/nbackup/internal/record"
)

// MemberIndex is the server-side cache of per-archive indexes (member lists + frame
// tables), kept as its own gzip files under the workdir so the catalog cache itself stays
// small and a scan/rebuild reads only commit footers. Indexes are loaded on demand
// (browse, structural verify, ranged reads) and cached here; the durable copy is each
// archive's on-medium index (record.KindIndex), which the loader falls back to (and
// repopulates this cache) on a miss.
type MemberIndex struct{ dir string }

var indexSlug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// OpenMemberIndex returns the member-index cache rooted at workdir/member-index.
func OpenMemberIndex(workdir string) *MemberIndex {
	return &MemberIndex{dir: filepath.Join(workdir, "member-index")}
}

func (m *MemberIndex) path(run, dle string, level int) string {
	name := indexSlug.ReplaceAllString(run, "_") + "__" +
		indexSlug.ReplaceAllString(dle, "_") + "__L" + strconv.Itoa(level) + ".idx.gz"
	return filepath.Join(m.dir, name)
}

// Store writes an archive's index to the cache (atomic temp+rename).
func (m *MemberIndex) Store(run, dle string, level int, idx record.Index) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := record.EncodeIndex(&buf, idx); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(m.path(run, dle, level), buf.Bytes(), 0o644)
}

// Load reads an archive's cached index, reporting whether it was present. A cache
// file that does not decode — a pre-shapes cache entry (the old bare-array format), or
// a torn write — is a MISS, not an error: this is only a cache, the durable copy is
// the archive's on-medium index, and an old workdir must degrade (unbrowsable) rather
// than fail the operations that touch it.
func (m *MemberIndex) Load(run, dle string, level int) (record.Index, bool, error) {
	f, err := os.Open(m.path(run, dle, level))
	if err != nil {
		if os.IsNotExist(err) {
			return record.Index{}, false, nil
		}
		return record.Index{}, false, err
	}
	defer f.Close()
	idx, err := record.DecodeIndex(f)
	if err != nil {
		return record.Index{}, false, nil
	}
	return idx, true, nil
}
