package fslike

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Niloen/nbackup/internal/record"
)

// memStore is an in-memory fslike.Store for exercising the layer's concurrency seams.
// onWrite, if set, is invoked from Write after the payload bytes are stored but before
// Write returns — a hook to widen the window between "payload on disk" and "position
// indexed" that AppendFile straddles.
type memStore struct {
	mu      sync.Mutex
	files   map[string][]byte
	onWrite func(key string)
}

func newMemStore() *memStore { return &memStore{files: map[string][]byte{}} }

func (s *memStore) Key(slot, name string) string { return slot + "/" + name }

func (s *memStore) Writer(_ context.Context, key string) (io.WriteCloser, error) {
	return &memWriter{s: s, key: key}, nil
}

type memWriter struct {
	s   *memStore
	key string
	buf bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memWriter) Close() error {
	w.s.mu.Lock()
	w.s.files[w.key] = append([]byte(nil), w.buf.Bytes()...)
	w.s.mu.Unlock()
	if w.s.onWrite != nil {
		w.s.onWrite(w.key)
	}
	return nil
}

func (s *memStore) WriteAll(_ context.Context, key string, b []byte) error {
	s.mu.Lock()
	s.files[key] = append([]byte(nil), b...)
	s.mu.Unlock()
	return nil
}

func (s *memStore) ReadAll(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[key]
	if !ok {
		return nil, io.EOF
	}
	return append([]byte(nil), b...), nil
}

func (s *memStore) Open(key string) (io.ReadCloser, error) {
	b, err := s.ReadAll(key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStore) List() ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Object
	for key := range s.files {
		slot, name, _ := strings.Cut(key, "/")
		out = append(out, Object{Key: key, Slot: slot, Base: name})
	}
	return out, nil
}

func (s *memStore) RemoveTree(slot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.files {
		if strings.HasPrefix(key, slot+"/") {
			delete(s.files, key)
		}
	}
	return nil
}

func (s *memStore) Remove(key string) error {
	s.mu.Lock()
	delete(s.files, key)
	s.mu.Unlock()
	return nil
}

func appendArchive(t *testing.T, v *Volume, slot, dle, payload string) int {
	t.Helper()
	fw, err := v.AppendFile(context.Background(),
		record.Header{Slot: slot, Kind: record.KindArchive, DLE: dle, Level: 0, Compress: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	return fw.Pos()
}

// TestReclaimSparesInFlightAppend reproduces the holding-disk corruption: dumpers
// append into one slot directory while the drain reclaims drained archives from the
// same slot. When a reclaim removed a slot's last *indexed* file, it used to RemoveTree
// the directory based on the lagging in-memory index — destroying a payload another
// dumper had just written but not yet indexed. The position-reservation in AppendFile
// must make the reclaim see the in-flight append and spare its directory.
func TestReclaimSparesInFlightAppend(t *testing.T) {
	st := newMemStore()
	v, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	// A previously drained archive's leftover file, the reclaim's target.
	posA := appendArchive(t, v, "slot-x", "done", "AAA")

	// A second dumper begins appending into the same slot. Block inside Write — the
	// in-flight payload is on disk but not yet finalized — until the reclaim has run.
	reached := make(chan struct{})
	release := make(chan struct{})
	st.onWrite = func(key string) {
		if strings.Contains(key, "inflight") {
			close(reached)
			<-release
		}
	}
	done := make(chan int, 1)
	go func() { done <- appendArchive(t, v, "slot-x", "inflight", "BBB") }()

	<-reached
	// The drain reclaims slot-x's last indexed file while the second append is mid-flight.
	if err := v.RemoveFile(posA); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	close(release)
	posB := <-done

	// The in-flight payload must have survived the concurrent reclaim.
	_, rc, err := v.ReadFile(posB)
	if err != nil {
		t.Fatalf("in-flight payload destroyed by concurrent reclaim: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "BBB" {
		t.Fatalf("payload = %q, want %q", got, "BBB")
	}
}

// TestIncompleteFiles pins the torn-append enumeration the prune sweep relies on: a
// position with exactly one of its payload/.hdr pair present (an interrupted append) is
// reported; a well-formed pair is not. A both-empty in-flight reservation cannot survive
// a reopen (scan rebuilds only from stored objects), so the reopened index has none.
func TestIncompleteFiles(t *testing.T) {
	st := newMemStore()
	v, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	appendArchive(t, v, "slot-x", "app", "AAA") // a complete pair (payload + .hdr)
	// A torn append: a payload object at a conforming position with no .hdr sidecar.
	if err := st.WriteAll(context.Background(), "slot-x/7-torn.tar", []byte("BBB")); err != nil {
		t.Fatal(err)
	}
	// Reopen so scan() reindexes from the store and marks the torn position incomplete.
	v2, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.IncompleteFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("IncompleteFiles = %v, want [7] (the torn file only, not the complete pair)", got)
	}
}

func TestPayloadExtEncryption(t *testing.T) {
	cases := []struct {
		name    string
		h       record.Header
		wantExt string
	}{
		{"plain none", record.Header{Kind: record.KindArchive, Compress: "none"}, ".tar"},
		{"plain gzip", record.Header{Kind: record.KindArchive, Compress: "gzip"}, ".tar.gz"},
		{"plain zstd", record.Header{Kind: record.KindArchive, Compress: "zstd"}, ".tar.zst"},
		{"encrypted none", record.Header{Kind: record.KindArchive, Compress: "none", Encrypt: "gpg"}, ".tar.gpg"},
		{"encrypted gzip", record.Header{Kind: record.KindArchive, Compress: "gzip", Encrypt: "gpg"}, ".tar.gz.gpg"},
		{"encrypted zstd", record.Header{Kind: record.KindArchive, Compress: "zstd", Encrypt: "gpg"}, ".tar.zst.gpg"},
		{"encrypt none-string stays plain", record.Header{Kind: record.KindArchive, Compress: "gzip", Encrypt: "none"}, ".tar.gz"},
		// The commit footer and member index are never encrypted — they keep plaintext names.
		{"commit ignores encrypt", record.Header{Kind: record.KindCommit, Compress: "gzip", Encrypt: "gpg"}, ".json"},
		{"index ignores encrypt", record.Header{Kind: record.KindIndex, Compress: "gzip", Encrypt: "gpg"}, ".json.gz"},
		// A split archive appends a .pNNN part-index suffix AFTER the payload extension, so a
		// fragment never poses as a directly-openable .tar.gz/.gpg. Set even for a sole part 0.
		{"split part 0", record.Header{Kind: record.KindArchive, Compress: "gzip", Split: true}, ".tar.gz.p000"},
		{"split part 1", record.Header{Kind: record.KindArchive, Compress: "gzip", Split: true, Part: 1}, ".tar.gz.p001"},
		{"split encrypted part 2", record.Header{Kind: record.KindArchive, Compress: "zstd", Encrypt: "gpg", Split: true, Part: 2}, ".tar.zst.gpg.p002"},
		// Split rides only on the payload; the commit/index records keep their plaintext names.
		{"split commit stays plain", record.Header{Kind: record.KindCommit, Split: true, Part: 1}, ".json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := payloadExt(c.h); got != c.wantExt {
				t.Errorf("payloadExt = %q, want %q", got, c.wantExt)
			}
		})
	}
}
