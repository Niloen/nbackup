// Package mount serves the catalog as a read-only FUSE filesystem: the top
// level lists runs, and each run holds every DLE's snapshot as of that run —
// the DLE's most recent dump at or before the run plus the incrementals up to
// it, exactly the view `nb recover` browses for a date, pinned to a run.
//
// Browsing works over the member indexes (no media reads on a cached browse).
// A file's content is recovered on first open: the file is extracted through
// the normal selected-file recovery path into a cache directory and served
// from there, so a file's size reads 0 until it is first opened (open handles
// use direct I/O, so `cat` and `cp` see the full content regardless).
//
// The view carries file-level recovery's fidelity caveat: it is a union
// (most-recent-wins per path), so a file deleted before the run may still
// appear; a deletion-accurate rebuild is `nb recover --all`.
package mount

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/record"
	"github.com/Niloen/nbackup/internal/recovery"
)

// Backend is what the mount needs from the engine, as plain funcs so the
// filesystem is testable without one: the catalog's runs, a run-pinned browse
// tree per DLE, and selected-file extraction (plus delta-tipped assemblies)
// into a directory.
type Backend struct {
	Runs    func() []*catalog.Run
	Tree    func(dle, runID string) (*recovery.Tree, error)
	Extract func(steps []recovery.ExtractStep, asms []recovery.Assembly, destDir string) error
}

// Options configures a mount. The caller owns CacheDir's lifecycle — the mount
// only writes recovered files under it (<cache>/<run>/<dle>/<path>).
type Options struct {
	Mountpoint string
	CacheDir   string
	Logf       func(format string, args ...any)
}

// Serve mounts the filesystem and returns the running server. The caller
// Wait()s on it; Unmount() stops it.
func Serve(opts Options, b Backend) (*fuse.Server, error) {
	m := &mountFS{b: b, cacheDir: opts.CacheDir, logf: opts.Logf}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	// Attrs are never cached by the kernel: a file's size flips from 0 to real
	// on its first open, and a cached zero would truncate a reader's view.
	attrTimeout := time.Duration(0)
	entryTimeout := time.Second
	return fs.Mount(opts.Mountpoint, &rootNode{m: m}, &fs.Options{
		MountOptions: fuse.MountOptions{FsName: "nbackup", Name: "nbackup", Options: []string{"ro"}},
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
	})
}

// mountFS is the state shared by every node: the backend, the recovered-file
// cache root, and the extraction lock — extractions run one at a time, the
// mount's single owner of media access.
type mountFS struct {
	b        Backend
	cacheDir string
	logf     func(string, ...any)

	mu sync.Mutex // serializes extractions
}

// run finds a catalog run by id.
func (m *mountFS) run(id string) (*catalog.Run, bool) {
	for _, r := range m.b.Runs() {
		if r.ID == id {
			return r, true
		}
	}
	return nil, false
}

// dlesAt returns the DLE slugs with any archive at or before the run, sorted —
// a run's snapshot shows every DLE's state as of that time, not only the DLEs
// the run itself dumped.
func (m *mountFS) dlesAt(runID string) []string {
	seen := map[string]bool{}
	for _, r := range m.b.Runs() {
		if record.RunIDLess(runID, r.ID) { // r.ID > runID
			continue
		}
		for _, a := range r.Archives {
			seen[a.DLE] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// runMtime is the timestamp shown on a run's nodes: the instant encoded in the
// run id, or zero when it does not parse (a rebuilt legacy id).
func runMtime(runID string) time.Time {
	t, err := record.IDTime(runID, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// stamp fills an attr's times and read-only mode for a node of the given kind.
func stamp(a *fuse.Attr, dir bool, mtime time.Time) {
	if dir {
		a.Mode = fuse.S_IFDIR | 0o555
	} else {
		a.Mode = fuse.S_IFREG | 0o444
	}
	if !mtime.IsZero() {
		a.SetTimes(&mtime, &mtime, &mtime)
	}
}

// rootNode lists the catalog's runs.
type rootNode struct {
	fs.Inode
	m *mountFS
}

var (
	_ = (fs.NodeReaddirer)((*rootNode)(nil))
	_ = (fs.NodeLookuper)((*rootNode)(nil))
)

func (r *rootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	runs := r.m.b.Runs()
	out := make([]fuse.DirEntry, 0, len(runs))
	for _, run := range runs {
		out = append(out, fuse.DirEntry{Mode: fuse.S_IFDIR, Name: run.ID})
	}
	return fs.NewListDirStream(out), 0
}

func (r *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if _, ok := r.m.run(name); !ok {
		return nil, syscall.ENOENT
	}
	stamp(&out.Attr, true, runMtime(name))
	return r.NewInode(ctx, &runNode{m: r.m, runID: name}, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

// runNode is one run's directory, listing the DLEs it has a snapshot for.
type runNode struct {
	fs.Inode
	m     *mountFS
	runID string
}

var (
	_ = (fs.NodeReaddirer)((*runNode)(nil))
	_ = (fs.NodeLookuper)((*runNode)(nil))
	_ = (fs.NodeGetattrer)((*runNode)(nil))
)

func (r *runNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	stamp(&out.Attr, true, runMtime(r.runID))
	return 0
}

func (r *runNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	dles := r.m.dlesAt(r.runID)
	out := make([]fuse.DirEntry, 0, len(dles))
	for _, d := range dles {
		out = append(out, fuse.DirEntry{Mode: fuse.S_IFDIR, Name: d})
	}
	return fs.NewListDirStream(out), 0
}

func (r *runNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, d := range r.m.dlesAt(r.runID) {
		if d == name {
			stamp(&out.Attr, true, runMtime(r.runID))
			return r.NewInode(ctx, &dleNode{m: r.m, runID: r.runID, dle: name}, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
		}
	}
	return nil, syscall.ENOENT
}

// dleNode is a DLE's snapshot root within a run. Its browse tree is built
// lazily on first access (that may read on-medium indexes on a cache miss)
// and kept for the mount's lifetime.
type dleNode struct {
	fs.Inode
	m          *mountFS
	runID, dle string

	once sync.Once
	tree *recovery.Tree
	err  error
}

var (
	_ = (fs.NodeReaddirer)((*dleNode)(nil))
	_ = (fs.NodeLookuper)((*dleNode)(nil))
	_ = (fs.NodeGetattrer)((*dleNode)(nil))
)

func (d *dleNode) load() (*recovery.Tree, syscall.Errno) {
	d.once.Do(func() { d.tree, d.err = d.m.b.Tree(d.dle, d.runID) })
	if d.err != nil {
		d.m.logf("mount: open %s/%s: %v", d.runID, d.dle, d.err)
		return nil, syscall.EIO
	}
	return d.tree, 0
}

// node resolves a clean tree path ("" = the DLE root) to its recovery node.
func (d *dleNode) node(p string) (*recovery.Node, syscall.Errno) {
	tree, errno := d.load()
	if errno != 0 {
		return nil, errno
	}
	n, ok := tree.Lookup(p)
	if !ok {
		return nil, syscall.ENOENT
	}
	return n, 0
}

func (d *dleNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	stamp(&out.Attr, true, runMtime(d.runID))
	return 0
}

func (d *dleNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	n, errno := d.node("")
	if errno != 0 {
		return nil, errno
	}
	return listChildren(n), 0
}

func (d *dleNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return lookupChild(ctx, &d.Inode, d, "", name, out)
}

// listChildren renders a recovery node's entries as a directory stream.
func listChildren(n *recovery.Node) fs.DirStream {
	cs := n.Children()
	out := make([]fuse.DirEntry, 0, len(cs))
	for _, c := range cs {
		out = append(out, fuse.DirEntry{Mode: nodeMode(c), Name: c.Name()})
	}
	return fs.NewListDirStream(out)
}

// nodeMode maps a tree node to its FUSE type.
func nodeMode(n *recovery.Node) uint32 {
	if n.IsDir() {
		return fuse.S_IFDIR
	}
	return fuse.S_IFREG
}

// lookupChild resolves one name under a tree directory into an inode — the
// shared Lookup of the DLE root and every directory below it.
func lookupChild(ctx context.Context, parent *fs.Inode, d *dleNode, dirPath, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	c, errno := d.node(path.Join(dirPath, name))
	if errno != 0 {
		return nil, errno
	}
	child := &treeEntry{d: d, node: c}
	child.fillAttr(&out.Attr)
	return parent.NewInode(ctx, child, fs.StableAttr{Mode: nodeMode(c)}), 0
}

// treeEntry is a file or directory inside a DLE's snapshot.
type treeEntry struct {
	fs.Inode
	d    *dleNode
	node *recovery.Node
}

var (
	_ = (fs.NodeReaddirer)((*treeEntry)(nil))
	_ = (fs.NodeLookuper)((*treeEntry)(nil))
	_ = (fs.NodeGetattrer)((*treeEntry)(nil))
	_ = (fs.NodeOpener)((*treeEntry)(nil))
)

// fillAttr stamps the entry's attributes; a file already recovered into the
// cache reports its real size, an unopened one reports 0 (content is only
// known after extraction — open handles use direct I/O so reads are not
// clamped by the 0).
func (e *treeEntry) fillAttr(a *fuse.Attr) {
	stamp(a, e.node.IsDir(), runMtime(e.d.runID))
	if e.node.IsDir() {
		return
	}
	if fi, err := os.Lstat(e.d.cachePath(e.node)); err == nil && fi.Mode().IsRegular() {
		a.Size = uint64(fi.Size())
		mt := fi.ModTime()
		a.SetTimes(&mt, &mt, &mt)
	}
}

func (e *treeEntry) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	e.fillAttr(&out.Attr)
	return 0
}

func (e *treeEntry) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if !e.node.IsDir() {
		return nil, syscall.ENOTDIR
	}
	return listChildren(e.node), 0
}

func (e *treeEntry) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !e.node.IsDir() {
		return nil, syscall.ENOTDIR
	}
	return lookupChild(ctx, &e.Inode, e.d, e.node.Path(), name, out)
}

func (e *treeEntry) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if e.node.IsDir() {
		return nil, 0, syscall.EISDIR
	}
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	p, err := e.d.ensure(e.node)
	if err != nil {
		e.d.m.logf("mount: recover %s/%s/%s: %v", e.d.runID, e.d.dle, e.node.Path(), err)
		return nil, 0, syscall.EIO
	}
	f, err := os.Open(p)
	if err != nil {
		e.d.m.logf("mount: open cached %s: %v", p, err)
		return nil, 0, syscall.EIO
	}
	// Direct I/O: the kernel must not clamp reads to a size it may have seen
	// as 0 before the extraction, nor cache pages of a lazily-appearing file.
	return &fileHandle{f: f}, fuse.FOPEN_DIRECT_IO, 0
}

// cacheDirFor is where a DLE snapshot's recovered files live.
func (d *dleNode) cacheDirFor() string {
	return filepath.Join(d.m.cacheDir, d.runID, d.dle)
}

// cachePath is a tree node's recovered-file location in the cache.
func (d *dleNode) cachePath(n *recovery.Node) string {
	return filepath.Join(d.cacheDirFor(), filepath.FromSlash(n.Path()))
}

// ensure recovers a file into the cache on its first open and returns the
// cached path; later opens are served straight from the cache. Extraction is
// the normal selected-file recovery (union view, never deletes), one at a time.
func (d *dleNode) ensure(n *recovery.Node) (string, error) {
	p := d.cachePath(n)
	d.m.mu.Lock()
	defer d.m.mu.Unlock()
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	tree, errno := d.load()
	if errno != 0 {
		return "", d.err
	}
	steps, asms, err := tree.Collect([]string{n.Path()})
	if err != nil {
		return "", err
	}
	if err := d.m.b.Extract(steps, asms, d.cacheDirFor()); err != nil {
		return "", err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("recovered, but %s is missing from the output: %w", n.Path(), err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file in the archive (%v) — recover it with `nb recover`", n.Path(), fi.Mode())
	}
	return p, nil
}

// fileHandle serves reads from a recovered file in the cache.
type fileHandle struct {
	f *os.File
}

var (
	_ = (fs.FileReader)((*fileHandle)(nil))
	_ = (fs.FileReleaser)((*fileHandle)(nil))
)

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	h.f.Close()
	return 0
}
