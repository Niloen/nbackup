// Package crypt runs stream encryptors/decryptors as external child processes,
// the way package filter orchestrates compressors and the way Amanda drives an
// encrypt program (amgpgcrypt/amcrypt). NBackup stays a thin driver: it pipes
// bytes through a child and lets a proven tool (gpg) own the cryptography and the
// key material — NBackup never touches a key.
//
// A scheme is a registered name (gpg, none) that knows how to build the argv for
// encrypting and decrypting. The archive records which scheme produced it (the
// name only, never a key), so restore reverses the exact transform — resolved
// from this compiled registry, not from config, so a slot restores even with the
// config gone. It is the peer of a filter codec, one transform further out: on
// write a payload is compressed and then encrypted; on read it is decrypted and
// then decompressed.
package crypt

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Options tune a scheme invocation. They carry a key *reference* (a gpg recipient
// or a passphrase-file path), never key bytes; the actual secret lives in the
// operator's gpg keyring/keystore, which gpg resolves itself.
type Options struct {
	Program        string // override the scheme's default binary (e.g. an absolute path); "" = default
	Recipient      string // gpg public-key recipient (asymmetric encrypt); decrypt needs none (key auto-discovered)
	PassphraseFile string // path to a passphrase file (gpg symmetric); used on both encrypt and decrypt
	Nice           int    // run the child under `nice -n Nice` for CPU politeness; 0 = no nice
}

// Spec describes a scheme: how to build the child argv for encrypting and
// decrypting. A nil argv builder means "no external process" (the none scheme).
type Spec struct {
	Name         string
	encryptArgv  func(o Options) []string
	decryptArgv  func(o Options) []string
	needsKeyHint string // non-empty for schemes that require a key reference (used by Check)
}

var registry = map[string]Spec{}

func register(s Spec) { registry[s.Name] = s }

func init() {
	register(Spec{
		Name: "gpg",
		encryptArgv: func(o Options) []string {
			argv := []string{prog(o, "gpg"), "--batch", "--yes", "--no-tty", "--compress-algo", "none"}
			if o.Recipient != "" { // public-key (asymmetric)
				argv = append(argv, "-e", "-r", o.Recipient)
			} else { // symmetric, passphrase from a file
				argv = append(argv, "--pinentry-mode", "loopback", "--passphrase-file", o.PassphraseFile, "-c")
			}
			return append(argv, "--output", "-")
		},
		decryptArgv: func(o Options) []string {
			argv := []string{prog(o, "gpg"), "--batch", "--yes", "--no-tty"}
			if o.PassphraseFile != "" { // symmetric: supply the passphrase non-interactively
				argv = append(argv, "--pinentry-mode", "loopback", "--passphrase-file", o.PassphraseFile)
			}
			// Public-key decrypt needs no key reference: gpg reads the recipient key-id
			// from the ciphertext and finds the matching secret key in the keyring.
			return append(argv, "-d")
		},
		needsKeyHint: "gpg needs a `recipient` (public-key) or a `passphrase_file` (symmetric)",
	})
	register(Spec{Name: "none"}) // identity: no child process
}

func prog(o Options, def string) string {
	if o.Program != "" {
		return o.Program
	}
	return def
}

func spec(scheme string) (Spec, error) {
	if scheme == "" {
		scheme = "none" // an unset Encryption field means the archive is plaintext
	}
	s, ok := registry[scheme]
	if !ok {
		return Spec{}, fmt.Errorf("unknown encryption scheme %q (known: %s)", scheme, strings.Join(names(), ", "))
	}
	return s, nil
}

func names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Check verifies the scheme is known, its binary is available on PATH, and any
// required key reference is present. It does not test that a key actually
// decrypts — only that the run is configured well enough to start.
func Check(scheme string, o Options) error {
	s, err := spec(scheme)
	if err != nil {
		return err
	}
	if s.encryptArgv == nil {
		return nil // none: nothing to run
	}
	if s.needsKeyHint != "" && o.Recipient == "" && o.PassphraseFile == "" {
		return fmt.Errorf("encryption scheme %q: %s", scheme, s.needsKeyHint)
	}
	// Fail fast on a passphrase file that isn't there: otherwise gpg only fails
	// once bytes start flowing, surfacing as a broken-pipe mid-dump.
	if o.PassphraseFile != "" {
		if _, err := os.Stat(o.PassphraseFile); err != nil {
			return fmt.Errorf("encryption scheme %q: passphrase_file %q is unreadable: %w", scheme, o.PassphraseFile, err)
		}
	}
	bin := s.encryptArgv(o)[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("encryption scheme %q needs %q on PATH: %w", scheme, bin, err)
	}
	return nil
}

// Encrypt returns a WriteCloser that pipes everything written to it through the
// scheme's encryptor child and on to dst (the bytes that land on the volume).
// Close finishes and waits the child.
func Encrypt(scheme string, dst io.Writer, o Options) (io.WriteCloser, error) {
	s, err := spec(scheme)
	if err != nil {
		return nil, err
	}
	if s.encryptArgv == nil {
		return nopWriteCloser{dst}, nil
	}
	cmd := command(s.encryptArgv(o), o)
	cmd.Stdout = dst
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	return &procWriter{cmd: cmd, stdin: stdin, stderr: &stderr}, nil
}

// Decrypt returns a ReadCloser that yields the plaintext form of src by piping it
// through the scheme's decryptor child. Close waits the child. This is the only
// step that needs the key; verify, copy, and browse all operate on ciphertext.
func Decrypt(scheme string, src io.Reader, o Options) (io.ReadCloser, error) {
	s, err := spec(scheme)
	if err != nil {
		return nil, err
	}
	if s.decryptArgv == nil {
		return io.NopCloser(src), nil
	}
	cmd := command(s.decryptArgv(o), o)
	cmd.Stdin = src
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	return &procReader{cmd: cmd, stdout: stdout, stderr: &stderr}, nil
}

// command builds the exec.Cmd, prefixing `nice` for politeness when requested.
func command(argv []string, o Options) *exec.Cmd {
	if o.Nice != 0 {
		argv = append([]string{"nice", "-n", strconv.Itoa(o.Nice)}, argv...)
	}
	return exec.Command(argv[0], argv[1:]...)
}

type procWriter struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *strings.Builder
}

func (p *procWriter) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *procWriter) Close() error {
	stdinErr := p.stdin.Close()
	waitErr := p.cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("%s: %w\n%s", p.cmd.Path, waitErr, strings.TrimSpace(p.stderr.String()))
	}
	return stdinErr
}

type procReader struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *strings.Builder
}

func (p *procReader) Read(b []byte) (int, error) { return p.stdout.Read(b) }

func (p *procReader) Close() error {
	p.stdout.Close()
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w\n%s", p.cmd.Path, err, strings.TrimSpace(p.stderr.String()))
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
