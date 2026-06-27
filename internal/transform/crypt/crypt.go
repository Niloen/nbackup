// Package crypt runs stream encryptors/decryptors as external child processes,
// the way package compress orchestrates compressors. NBackup stays a thin
// driver: it pipes
// bytes through a child and lets a proven tool (gpg) own the cryptography and the
// key material — NBackup never touches a key.
//
// A scheme is a registered name (gpg, none) that knows how to build the argv for
// encrypting and decrypting. The archive records which scheme produced it (the
// name only, never a key), so restore reverses the exact transform — resolved
// from this compiled registry, not from config, so a slot restores even with the
// config gone. It is the peer of a compression codec, one transform further out: on
// write a payload is compressed and then encrypted; on read it is decrypted and
// then decompressed.
package crypt

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Niloen/nbackup/internal/programs"
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
		return Spec{}, fmt.Errorf("unknown encryption scheme %q (known: %s)", scheme, strings.Join(sortedNames(registry), ", "))
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

// EncryptCmd returns the encryptor as a pipeline stage, or ok=false for the identity
// (none) scheme. It lets the unified pipeline run encryption through any executor — on
// the client when the key lives there, so plaintext never leaves it.
func EncryptCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.encryptArgv }, o)
}

// DecryptCmd returns the decryptor as a pipeline stage (the read-side peer of
// EncryptCmd), or ok=false for none. This is the only stage that needs the key.
func DecryptCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return stageCmd(scheme, func(s Spec) func(Options) []string { return s.decryptArgv }, o)
}

// Filter returns the scheme as a reversible programs.Filter — Forward encrypts, Reverse
// decrypts — for the transform layer to place and chain. The none scheme yields a Filter
// with empty cmds (skipped by the pipeline). It errors only for an unknown scheme.
func Filter(scheme string, o Options) (programs.Filter, error) {
	fwd, _, err := EncryptCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	rev, _, err := DecryptCmd(scheme, o)
	if err != nil {
		return programs.Filter{}, err
	}
	return programs.Filter{Name: scheme, Forward: fwd, Reverse: rev}, nil
}

func stageCmd(scheme string, pick func(Spec) func(Options) []string, o Options) (programs.Cmd, bool, error) {
	s, err := spec(scheme)
	if err != nil {
		return programs.Cmd{}, false, err
	}
	build := pick(s)
	if build == nil {
		return programs.Cmd{}, false, nil
	}
	argv := build(o)
	return programs.Cmd{Name: argv[0], Args: argv[1:], Nice: o.Nice}, true, nil
}

// sortedNames returns a registry map's keys sorted, for stable "known: …" errors.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
