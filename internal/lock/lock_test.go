package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireExclusive(t *testing.T) {
	dir := t.TempDir()

	l1, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// A second acquire on the same workdir must fail fast with ErrHeld.
	if _, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire: got %v, want ErrHeld", err)
	}

	// After release the lock is available again.
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestAcquireCreatesWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	l, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	if _, err := os.Stat(filepath.Join(dir, LockFile)); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

func TestReleaseNilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}
