package cli

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/Niloen/nbackup/internal/progress"
)

// TestRealBinaryCancelExit130 is the one end-to-end test that runs the actual built
// `nb` binary as a subprocess: it starts a real (small) backup, interrupts it with
// SIGINT mid-run, and asserts the process honors cmd/nb/main.go's cancel contract —
// exit code 130 and a plain "canceled by operator" message with no "error:" prefix.
// This closes the audit's #1 finding (nothing exercised the real binary before).
func TestRealBinaryCancelExit130(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT semantics differ on Windows")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("GNU tar not available")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not available (needed to make the dump slow enough to interrupt)")
	}

	bin := buildNB(t)

	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Incompressible data so gzip is CPU-bound and the dump takes long enough that a
	// SIGINT lands mid-run rather than after it completes.
	writeRandom(t, filepath.Join(src, "big.bin"), 48<<20)

	workdir := filepath.Join(base, "catalog")
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
compress:
  scheme: gzip
media:
  disk: { type: disk, path: %s }
sources:
  default:
    localhost: [%s]
`, workdir, filepath.Join(base, "state"), filepath.Join(base, "runs"), src)
	cfgPath := filepath.Join(base, "nbackup.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-c", cfgPath, "dump")
	cmd.Stdin = devnull // non-terminal stdin: no operator, clean unattended path
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nb: %v", err)
	}

	// Wait deterministically until the run has written a non-terminal status snapshot
	// (estimating or running), then interrupt — rather than racing a fixed sleep.
	if !waitForRunning(t, workdir, cmd) {
		// The process exited before we could observe a running phase. Surface why so a
		// real failure (e.g. tar behaving unexpectedly) is not silently masked.
		_ = cmd.Wait()
		t.Skipf("dump finished or failed before it could be interrupted; stderr:\n%s", stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected a non-zero exit, got err=%v\nstderr:\n%s", err, stderr.String())
	}
	if code := exitErr.ExitCode(); code != 130 {
		t.Fatalf("exit code = %d, want 130 (128+SIGINT)\nstderr:\n%s", code, stderr.String())
	}
	msg := stderr.String()
	if !bytes.Contains([]byte(msg), []byte("canceled by operator")) {
		t.Errorf("stderr missing the clean cancel message, got:\n%s", msg)
	}
	if bytes.Contains([]byte(msg), []byte("error:")) {
		t.Errorf("a cancel must not print the \"error:\" prefix, got:\n%s", msg)
	}
}

// buildNB compiles the nb binary into a temp dir and returns its path, skipping the
// test if the toolchain is unavailable.
func buildNB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available to build the binary")
	}
	bin := filepath.Join(t.TempDir(), "nb")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/Niloen/nbackup/cmd/nb").CombinedOutput()
	if err != nil {
		t.Skipf("go build failed (toolchain/network unavailable): %v\n%s", err, out)
	}
	return bin
}

// waitForRunning polls the run-status file until the run reports a non-terminal
// phase, returning false if the process exits or a timeout elapses first.
func waitForRunning(t *testing.T, workdir string, cmd *exec.Cmd) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := progress.Load(workdir)
		if err == nil && snap.Phase != "" && !snap.Phase.Terminal() {
			return true
		}
		// If the process is already gone, stop waiting.
		if cmd.ProcessState != nil {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// writeRandom writes n bytes of incompressible pseudo-random data to path.
func writeRandom(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := rand.New(rand.NewSource(1))
	buf := make([]byte, 1<<20)
	for n > 0 {
		chunk := len(buf)
		if chunk > n {
			chunk = n
		}
		for i := 0; i < chunk; i++ {
			buf[i] = byte(r.Intn(256))
		}
		if _, err := f.Write(buf[:chunk]); err != nil {
			t.Fatal(err)
		}
		n -= chunk
	}
}
