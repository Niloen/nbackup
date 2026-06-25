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
	"strings"

	"github.com/Niloen/nbackup/internal/hostexec"
	"github.com/Niloen/nbackup/internal/streamproc"
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

func prog(o Options, def string) string { return streamproc.ProgramOr(o.Program, def) }

func spec(scheme string) (Spec, error) {
	if scheme == "" {
		scheme = "none" // an unset Encryption field means the archive is plaintext
	}
	s, ok := registry[scheme]
	if !ok {
		return Spec{}, fmt.Errorf("unknown encryption scheme %q (known: %s)", scheme, strings.Join(streamproc.SortedNames(registry), ", "))
	}
	return s, nil
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
	// recipient (public-key) and passphrase_file (symmetric) are mutually exclusive:
	// at encrypt time recipient silently wins, so accepting both would give asymmetric
	// encryption to an operator who configured — and safeguarded — a passphrase file.
	if o.Recipient != "" && o.PassphraseFile != "" {
		return fmt.Errorf("encryption scheme %q: set exactly one of `recipient` (public-key) or `passphrase_file` (symmetric), not both", scheme)
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

// Decrypt returns a ReadCloser that yields the plaintext form of src by piping it
// through the scheme's decryptor child. Close waits the child. This is the only
// step that needs the key; verify, copy, and browse all operate on ciphertext.
func Decrypt(scheme string, src io.Reader, o Options) (io.ReadCloser, error) {
	s, err := spec(scheme)
	if err != nil {
		return nil, err
	}
	return streamproc.ReadThrough(s.argv(s.decryptArgv, o), o.Nice, src)
}

// EncryptCmd returns the encryptor as a pipeline stage, or ok=false for the identity
// (none) scheme. It lets the unified pipeline run encryption through any executor — on
// the client when the key lives there, so plaintext never leaves it.
func EncryptCmd(scheme string, o Options) (cmd hostexec.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.encryptArgv }, o)
}

// DecryptCmd returns the decryptor as a pipeline stage (the read-side peer of
// EncryptCmd), or ok=false for none. This is the only stage that needs the key.
func DecryptCmd(scheme string, o Options) (cmd hostexec.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.decryptArgv }, o)
}

func stageCmd(scheme string, pick func(Spec) func(Options) []string, o Options) (hostexec.Cmd, bool, error) {
	s, err := spec(scheme)
	if err != nil {
		return hostexec.Cmd{}, false, err
	}
	build := pick(s)
	if build == nil {
		return hostexec.Cmd{}, false, nil
	}
	argv := build(o)
	return hostexec.Cmd{Name: argv[0], Args: argv[1:], Nice: o.Nice}, true, nil
}

// argv applies an argv builder, returning nil for the none scheme (no child) so
// streamproc runs the identity transform.
func (s Spec) argv(build func(Options) []string, o Options) []string {
	if build == nil {
		return nil
	}
	return build(o)
}
