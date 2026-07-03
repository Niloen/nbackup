package depot

// faces.go names the three faces of an opened medium. The Depot mints them — opening a
// medium for one face is where access is acquired (and, for writing, where the run
// window's exclusive claim is taken); Close releases it. Each face is deliberately
// narrow: code holding a ReadMedium cannot label, advance, or author onto the medium,
// because those methods do not exist on the type — the access rule is the method set,
// not a runtime check (see docs/design/catalog-window.md).

import (
	"io"
	"time"

	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/record"
)

// ReadMedium is a medium opened for reading archive data: mount a volume and read its
// files, nothing that can change one. It is the fs's mounting role plus lifecycle;
// opening is refused while a run window write-owns the medium, which is how a window
// reader fails over past a medium being written instead of mounting it mid-write.
type ReadMedium interface {
	ReadFileAt(volume string, epoch, pos int) (record.Header, io.ReadCloser, error)
	MountForRead(volume string, epoch int) error
	io.Closer
}

// WriteMedium is a medium opened for run authoring — the librarian's write face plus
// the medium's identity, so the write path has one source of truth for "which medium"
// (a Session carries the handle, not a loose name). Opening takes the window's
// exclusive write claim; Close releases it.
type WriteMedium interface {
	Name() string
	Volume() media.Volume
	Parallel() bool
	PrepareWrite(appendable bool, expect string, now time.Time, logf librarian.Logf) (volName string, epoch int, err error)
	Allocator(volume string, epoch int, appendable bool, partSize int64, now time.Time, logf librarian.Logf) *librarian.Allocator
	LazyDriveAllocators(appendable bool, expect string, partSize int64, now time.Time, logf librarian.Logf) []*librarian.Allocator
	io.Closer
}

// AdminMedium is the operator face — label, load, inventory, and the passive
// introspection reporting reads (posture, ledger, drill) need. It cannot author a run
// and cannot read archive data.
type AdminMedium interface {
	Label(name string, relabel, force bool, minAge time.Duration, now time.Time, logf librarian.Logf) error
	Load(target string, byLabel bool, logf librarian.Logf) error
	View() (librarian.View, error)
	Labeled() bool
	AppendOnly() bool
	Volume() media.Volume
	io.Closer
}
