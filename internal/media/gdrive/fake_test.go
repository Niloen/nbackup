package gdrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// fakeDrive is an in-memory driveAPI for tests: there is no gocloud driver and no official
// in-memory Google Drive (unlike the cloud medium's mem:// bucket), so the store is exercised
// against this tree of nodes. It models what the store depends on — opaque ids, folders that
// may hold same-named children, ranged reads, and recursive folder delete.
type fakeDrive struct {
	mu    sync.Mutex
	nodes map[string]*fakeNode
	seq   int
}

type fakeNode struct {
	id, name, parent string
	folder           bool
	data             []byte
}

// rootID is the fixed id of the fake's root folder — the "folder" a gdrive medium is
// configured with.
const rootID = "root"

func newFakeDrive() *fakeDrive {
	return &fakeDrive{nodes: map[string]*fakeNode{
		rootID: {id: rootID, name: "root", folder: true},
	}}
}

func (f *fakeDrive) newID() string {
	f.seq++
	return fmt.Sprintf("id-%d", f.seq)
}

func (f *fakeDrive) list(parentID string) ([]driveItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []driveItem
	for _, n := range f.nodes {
		if n.parent == parentID {
			out = append(out, driveItem{id: n.id, name: n.name, folder: n.folder})
		}
	}
	return out, nil
}

func (f *fakeDrive) mkdir(parentID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.newID()
	f.nodes[id] = &fakeNode{id: id, name: name, parent: parentID, folder: true}
	return id, nil
}

func (f *fakeDrive) upload(ctx context.Context, parentID, name string, r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	// A canceled ctx must not commit a file (the fslike abort contract): the real Drive
	// upload's Do(ctx) fails, and the fake mirrors that after draining the pipe.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.newID()
	f.nodes[id] = &fakeNode{id: id, name: name, parent: parentID, data: data}
	return id, nil
}

func (f *fakeDrive) openRange(id string, off, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok || n.folder {
		return nil, fmt.Errorf("fake: no file %s", id)
	}
	data := n.data
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	data = data[off:]
	if length > 0 && length < int64(len(data)) {
		data = data[:length]
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (f *fakeDrive) remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteRec(id)
	return nil
}

// deleteRec removes id and every descendant — Drive deletes a folder's contents with it.
func (f *fakeDrive) deleteRec(id string) {
	var children []string
	for cid, n := range f.nodes {
		if n.parent == id {
			children = append(children, cid)
		}
	}
	for _, cid := range children {
		f.deleteRec(cid)
	}
	delete(f.nodes, id)
}

// countFolders returns how many folders named name exist under parent — used to assert the
// store never mints a duplicate run folder under concurrency.
func (f *fakeDrive) countFolders(parentID, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, node := range f.nodes {
		if node.parent == parentID && node.folder && node.name == name {
			n++
		}
	}
	return n
}

// folderID returns the id of the folder named name directly under parent, or "".
func (f *fakeDrive) folderID(parentID, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, node := range f.nodes {
		if node.parent == parentID && node.folder && node.name == name {
			return node.id
		}
	}
	return ""
}
