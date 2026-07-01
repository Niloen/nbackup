package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/Niloen/nbackup/internal/record"
)

// MemberIndex is the server-side cache of per-archive member lists (filenames), kept as its
// own gzip files under the workdir so the catalog cache itself stays small and a scan/rebuild
// reads only commit footers. Members are loaded on demand (browse, structural verify) and
// cached here; the durable copy is each archive's on-medium index (record.KindIndex), which
// the loader falls back to (and repopulates this cache) on a miss.
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

// Store writes an archive's member list to the cache (atomic temp+rename).
func (m *MemberIndex) Store(run, dle string, level int, members []string) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	p := m.path(run, dle, level)
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := record.EncodeIndex(f, members); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads an archive's cached member list, reporting whether it was present.
func (m *MemberIndex) Load(run, dle string, level int) ([]string, bool, error) {
	f, err := os.Open(m.path(run, dle, level))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	members, err := record.DecodeIndex(f)
	if err != nil {
		return nil, false, err
	}
	return members, true, nil
}
