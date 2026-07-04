package restorer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform/crypt"

	_ "github.com/Niloen/nbackup/internal/archiver/gnutar"
)

// shaHex is the reference checksum of b, the way the writer records it.
func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// faultReader yields data then a hard read error at a chosen offset — a
// truncated/faulted media read, distinct from a clean stream that simply
// hashes to the wrong value.
type faultReader struct {
	data []byte
	off  int
	err  error
}

func (f *faultReader) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		return 0, f.err
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

func (f *faultReader) Close() error { return nil }

// TestVerifyChecksumMatch: a clean stream whose bytes hash to the recorded sha
// verifies true.
func TestVerifyChecksumMatch(t *testing.T) {
	body := []byte("the exact ciphertext that landed on the volume")
	r := New(testDeps(&fakeStore{}, nil))
	ok, err := r.VerifyChecksum(io.NopCloser(bytes.NewReader(body)), shaHex(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("a matching checksum must verify true")
	}
}

// TestVerifyChecksumMismatch is the safety-critical branch: a fully readable
// stream whose hash differs from the recorded sha (bit-rot on the medium) must
// report (false, nil) — a clean read, wrong hash — NOT a read fault. The
// verifier maps this to ClassIntegrity ("checksum mismatch"), so a false read
// error here would misclassify bit-rot.
func TestVerifyChecksumMismatch(t *testing.T) {
	body := []byte("these are the bytes actually on disk")
	r := New(testDeps(&fakeStore{}, nil))
	ok, err := r.VerifyChecksum(io.NopCloser(bytes.NewReader(body)), shaHex([]byte("what the seal expected")))
	if err != nil {
		t.Fatalf("a clean read whose hash differs must return a nil error (got %v)", err)
	}
	if ok {
		t.Fatal("a hash mismatch must verify false")
	}
}

// TestVerifyChecksumReadFaultClassification: a genuine mid-stream read fault (I/O
// error / truncation) during a checksum verify must surface as (false, err), not
// be swallowed as "clean read, hash differs". xfer.Transfer tags a Hash sink's
// mid-copy fault RoleSink (Hash's part writer is a hash.Hash, whose Write never
// errors, so a mid-copy RoleSink here can only be the upstream read faulting);
// VerifyChecksum only treats RoleCommit (the sink's own mismatch verdict) as a
// clean mismatch, so this read fault must propagate as a real error.
func TestVerifyChecksumReadFaultClassification(t *testing.T) {
	fr := &faultReader{data: []byte("partial bytes then EIO"), err: errors.New("input/output error")}
	r := New(testDeps(&fakeStore{}, nil))
	ok, err := r.VerifyChecksum(fr, shaHex([]byte("irrelevant")))
	if ok {
		t.Fatal("a faulted read must never verify true")
	}
	if err == nil {
		t.Fatal("a read fault must surface as a non-nil error, not be swallowed as a hash mismatch")
	}
	if !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("want the underlying I/O error to survive, got %v", err)
	}
}

// gnutarOrSkip builds a real gnutar archiver, skipping when tar is absent (per
// the repo's test-env convention).
func gnutarOrSkip(t *testing.T) archiver.Archiver {
	t.Helper()
	a, err := archiver.Open("gnutar", nil, programs.Local(), t.TempDir())
	if err != nil {
		t.Skipf("gnutar unavailable: %v", err)
	}
	if err := a.Check(); err != nil {
		t.Skipf("gnutar Check failed (tar not installed?): %v", err)
	}
	return a
}

func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestListMembersRealTar lists the members of a genuine (uncompressed,
// unencrypted) tar stream through the decode path — the structural verify half.
func TestListMembersRealTar(t *testing.T) {
	arch := gnutarOrSkip(t)
	raw := buildTar(t, map[string]string{"etc/hosts": "127.0.0.1 localhost\n", "var/log/app.log": "boot\n"})
	r := New(testDeps(&fakeStore{}, nil))
	members, err := r.ListMembers(io.NopCloser(bytes.NewReader(raw)), "none", "none", crypt.Options{}, arch)
	if err != nil {
		t.Fatalf("ListMembers over a valid tar: %v", err)
	}
	var paths []string
	for _, m := range members {
		paths = append(paths, m.Path)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "etc/hosts") || !strings.Contains(joined, "var/log/app.log") {
		t.Fatalf("members = %v, want both tar entries", members)
	}
}

// TestListMembersTruncatedStream: a genuinely corrupted archive — a real tar
// with its tail cut off — must fail the structural list (tar reports a broken
// archive), surfacing an error the verifier classifies rather than a silent
// short member list read as success.
func TestListMembersTruncatedStream(t *testing.T) {
	arch := gnutarOrSkip(t)
	raw := buildTar(t, map[string]string{"big/file": strings.Repeat("payload-block-", 4096)})
	truncated := raw[:len(raw)/3] // cut mid-archive: header promises bytes that never arrive
	r := New(testDeps(&fakeStore{}, nil))
	_, err := r.ListMembers(io.NopCloser(bytes.NewReader(truncated)), "none", "none", crypt.Options{}, arch)
	if err == nil {
		t.Fatal("listing a truncated tar must error, not read as a clean short archive")
	}
}
