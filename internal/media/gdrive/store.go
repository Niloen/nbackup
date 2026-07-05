package gdrive

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/Niloen/nbackup/internal/media"
	"github.com/Niloen/nbackup/internal/media/fslike"
)

// runsDir is the folder under the configured root that holds one subfolder per run,
// mirroring the disk/cloud media's runs/ subtree so a run streams between them unchanged.
const runsDir = "runs"

// driveStore is a fslike.Store over the Google Drive API. It gives Drive — which addresses
// files by opaque id and permits same-named siblings — the path-shaped storage fslike
// expects, by resolving each run-layout path (runs/<run>/<file>) to a Drive id and caching
// the mapping. Folder creation is serialized through mu so concurrent dumpers can't mint
// duplicate run folders; the uploads themselves run outside the lock.
type driveStore struct {
	api    driveAPI
	rootID string // the configured folder (or its prefix subfolder) all keys hang under

	mu    sync.Mutex
	dirs  map[string]string // folder path ("runs", "runs/<run>") -> Drive folder id
	files map[string]string // file key ("runs/<run>/<base>") -> Drive file id
}

func newStore(api driveAPI, rootID string) *driveStore {
	return &driveStore{api: api, rootID: rootID, dirs: map[string]string{}, files: map[string]string{}}
}

// Key builds the storage key for a file in a run: the same runs/<run>/<name> shape the
// cloud medium uses, so the on-Drive layout matches disk and cloud byte-for-byte.
func (s *driveStore) Key(run, name string) string { return path.Join(runsDir, run, name) }

// dirIDLocked resolves a slash-separated folder path (relative to rootID) to its Drive
// folder id, creating missing folders when create is set. Caller holds s.mu. Holding the
// lock across the list/mkdir round-trips is deliberate: it makes "look for the folder,
// else create it" atomic so two concurrent appends to a new run share one folder instead
// of racing to create two.
func (s *driveStore) dirIDLocked(dirPath string, create bool) (id string, ok bool, err error) {
	if dirPath == "" || dirPath == "." {
		return s.rootID, true, nil
	}
	if id, ok := s.dirs[dirPath]; ok {
		return id, true, nil
	}
	parent, name := path.Split(dirPath)
	parentID, ok, err := s.dirIDLocked(strings.TrimSuffix(parent, "/"), create)
	if err != nil || !ok {
		return "", false, err
	}
	items, err := s.api.list(parentID)
	if err != nil {
		return "", false, err
	}
	for _, it := range items {
		if it.folder && it.name == name {
			s.dirs[dirPath] = it.id
			return it.id, true, nil
		}
	}
	if !create {
		return "", false, nil
	}
	newID, err := s.api.mkdir(parentID, name)
	if err != nil {
		return "", false, err
	}
	s.dirs[dirPath] = newID
	return newID, true, nil
}

// resolveFileID finds the Drive id for a file key, listing (and caching) the parent
// folder's children on a cache miss. ok is false when the file — or its parent folder —
// does not exist, which the read/remove callers treat as "not there" rather than an error.
func (s *driveStore) resolveFileID(key string) (id string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.files[key]; ok {
		return id, true, nil
	}
	dir, _ := path.Split(key)
	dirPath := strings.TrimSuffix(dir, "/")
	parentID, ok, err := s.dirIDLocked(dirPath, false)
	if err != nil || !ok {
		return "", false, err
	}
	items, err := s.api.list(parentID)
	if err != nil {
		return "", false, err
	}
	for _, it := range items {
		if !it.folder {
			s.files[path.Join(dirPath, it.name)] = it.id
		}
	}
	id, ok = s.files[key]
	return id, ok, nil
}

// Writer streams a new file at key: it ensures the parent run folder exists, then uploads
// through an io.Pipe so the payload flows straight to Drive without buffering. Close commits
// (waits for the upload and caches the id); a canceled ctx aborts the upload, and because the
// fslike layer writes the payload before its .hdr sidecar, an aborted or completed-then-dropped
// upload is a sidecar-less orphan the scan ignores — the same atomicity disk and cloud rely on.
func (s *driveStore) Writer(ctx context.Context, key string) (io.WriteCloser, error) {
	dir, base := path.Split(key)
	s.mu.Lock()
	parentID, _, err := s.dirIDLocked(strings.TrimSuffix(dir, "/"), true)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	done := make(chan uploadResult, 1)
	go func() {
		id, err := s.api.upload(ctx, parentID, base, pr)
		// Unblock any pending/future Write on the pipe: on error surface it to the writer,
		// on success the reader has already drained to EOF.
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.Close()
		}
		done <- uploadResult{id: id, err: err}
	}()
	return &uploadWriter{store: s, key: key, pw: pw, done: done}, nil
}

type uploadResult struct {
	id  string
	err error
}

// uploadWriter is the payload writer for one in-flight Writer: Write streams to the pipe,
// Close signals EOF (committing the upload) and waits for the uploader goroutine.
type uploadWriter struct {
	store *driveStore
	key   string
	pw    *io.PipeWriter
	done  chan uploadResult
}

func (w *uploadWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *uploadWriter) Close() error {
	w.pw.Close() // EOF -> the uploader finishes (or a canceled ctx has already failed it)
	res := <-w.done
	if res.err != nil {
		return res.err
	}
	w.store.mu.Lock()
	w.store.files[w.key] = res.id
	w.store.mu.Unlock()
	return nil
}

// Open opens the rng slice of the file at key through Drive's ranged media download.
func (s *driveStore) Open(key string, rng media.Range) (io.ReadCloser, error) {
	id, ok, err := s.resolveFileID(key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("gdrive: no file at %s", key)
	}
	return s.api.openRange(id, rng.Off, rng.Len)
}

// List walks runs/<run>/<file> and returns every stored file for the catalog-rebuild
// scan, caching the folder and file ids it discovers. Under the drive.file scope Drive
// returns only app-created files, which is exactly the backup set this medium wrote.
func (s *driveStore) List() ([]fslike.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runsID, ok, err := s.dirIDLocked(runsDir, false)
	if err != nil || !ok {
		return nil, err // no runs/ folder yet: an empty volume
	}
	runFolders, err := s.api.list(runsID)
	if err != nil {
		return nil, err
	}
	var out []fslike.Object
	for _, rf := range runFolders {
		if !rf.folder {
			continue
		}
		s.dirs[path.Join(runsDir, rf.name)] = rf.id
		files, err := s.api.list(rf.id)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.folder {
				continue
			}
			key := path.Join(runsDir, rf.name, f.name)
			s.files[key] = f.id
			out = append(out, fslike.Object{Key: key, Run: rf.name, Base: f.name})
		}
	}
	return out, nil
}

// Remove deletes a single file by key; a missing file is a no-op (idempotent reclamation).
func (s *driveStore) Remove(key string) error {
	id, ok, err := s.resolveFileID(key)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.api.remove(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.files, key)
	s.mu.Unlock()
	return nil
}

// RemoveTree deletes a run's folder (Drive removes its descendants) and drops the cached
// ids beneath it. A missing run folder is a no-op.
func (s *driveStore) RemoveTree(run string) error {
	dirPath := path.Join(runsDir, run)
	s.mu.Lock()
	id, ok, err := s.dirIDLocked(dirPath, false)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.api.remove(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.dirs, dirPath)
	prefix := dirPath + "/"
	for k := range s.files {
		if strings.HasPrefix(k, prefix) {
			delete(s.files, k)
		}
	}
	s.mu.Unlock()
	return nil
}
