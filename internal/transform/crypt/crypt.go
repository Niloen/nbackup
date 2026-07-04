// Package crypt runs stream encryptors/decryptors as external child processes,
// the way package compress orchestrates compressors. NBackup stays a thin
// driver: it pipes
// bytes through a child and lets a proven tool (gpg) own the cryptography and the
// key material — NBackup never touches a key.
//
// A scheme is a registered name (gpg, none) that knows how to build the argv for
// encrypting and decrypting. The archive records which scheme produced it (the
// name only, never a key), so restore reverses the exact transform — resolved
// from this compiled registry, not from config, so a run restores even with the
// config gone. It is the peer of a compression scheme, one transform further out: on
// write a payload is compressed and then encrypted; on read it is decrypted and
// then decompressed.
package crypt

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform"
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

var registry = transform.NewRegistry[Options]("encryption", func(o Options) int { return o.Nice })

// keyHints holds, per scheme that requires a key reference, the hint Check
// reports when none is configured.
var keyHints = map[string]string{}

func init() {
	registry.Register(transform.Scheme[Options]{
		Name: "gpg",
		// PerFrame, not Full: GnuPG >= 2.2.8 deliberately refuses concatenated messages
		// (an anti-splicing measure), so each frame needs its own decrypt invocation.
		Concat: transform.ConcatPerFrame,
		Forward: func(o Options) []string {
			argv := []string{transform.Prog(o.Program, "gpg"), "--batch", "--yes", "--no-tty", "--compress-algo", "none"}
			if o.Recipient != "" { // public-key (asymmetric)
				argv = append(argv, "-e", "-r", o.Recipient)
			} else { // symmetric, passphrase from a file
				argv = append(argv, "--pinentry-mode", "loopback", "--passphrase-file", o.PassphraseFile, "-c")
			}
			return append(argv, "--output", "-")
		},
		Reverse: func(o Options) []string {
			argv := []string{transform.Prog(o.Program, "gpg"), "--batch", "--yes", "--no-tty"}
			if o.PassphraseFile != "" { // symmetric: supply the passphrase non-interactively
				argv = append(argv, "--pinentry-mode", "loopback", "--passphrase-file", o.PassphraseFile)
			}
			// Public-key decrypt needs no key reference: gpg reads the recipient key-id
			// from the ciphertext and finds the matching secret key in the keyring.
			return append(argv, "-d")
		},
	})
	keyHints["gpg"] = "gpg needs a `recipient` (public-key) or a `passphrase_file` (symmetric)"
	registry.Register(transform.Scheme[Options]{Name: "none", Concat: transform.ConcatFull}) // identity: no child process; concatenation is trivially one stream
}

// Concat returns the scheme's declared frame-composition capability (an unset
// scheme is the plaintext none).
func Concat(scheme string) (transform.Concat, error) {
	return registry.Concat(norm(scheme))
}

// norm maps an unset scheme to "none": an unset Encryption field means the
// archive is plaintext.
func norm(scheme string) string {
	if scheme == "" {
		return "none"
	}
	return scheme
}

// Check verifies the scheme is known, its binary is available on PATH, and any
// required key reference is present. It does not test that a key actually
// decrypts — only that the run is configured well enough to start.
func Check(scheme string, o Options) error {
	scheme = norm(scheme)
	s, err := registry.Lookup(scheme)
	if err != nil {
		return err
	}
	if s.Forward == nil {
		return nil // none: nothing to run
	}
	if hint := keyHints[scheme]; hint != "" && o.Recipient == "" && o.PassphraseFile == "" {
		return fmt.Errorf("encryption scheme %q: %s", scheme, hint)
	}
	// recipient (public-key) and passphrase_file (symmetric) are mutually exclusive:
	// at encrypt time recipient silently wins, so accepting both would give asymmetric
	// encryption to an operator who configured — and safeguarded — a passphrase file.
	if o.Recipient != "" && o.PassphraseFile != "" {
		return fmt.Errorf("encryption scheme %q: set exactly one of `recipient` (public-key) or `passphrase_file` (symmetric), not both", scheme)
	}
	// Fail fast on a passphrase file gpg can't read: a missing file, or one we
	// lack read permission for (e.g. mode 000). os.Stat would pass a mode-000
	// file — it only needs +x on the parent dir — so actually open it for reading,
	// the same access gpg will need, and let gpg's broken-pipe-mid-dump turn into
	// a clear check-time error.
	if o.PassphraseFile != "" {
		f, err := os.Open(o.PassphraseFile)
		if err != nil {
			return fmt.Errorf("encryption scheme %q: passphrase_file %q is unreadable: %w", scheme, o.PassphraseFile, err)
		}
		f.Close()
	}
	bin := s.Forward(o)[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("encryption scheme %q needs %q on PATH: %w", scheme, bin, err)
	}
	// For public-key encryption, confirm the recipient actually resolves to a key in
	// the keyring now — otherwise a typo'd recipient passes `nb check` and only fails
	// mid-dump with gpg's cryptic "No name"/WKD-lookup message.
	if o.Recipient != "" {
		if out, err := exec.Command(bin, "--batch", "--no-tty", "--list-keys", o.Recipient).CombinedOutput(); err != nil {
			return fmt.Errorf("encryption scheme %q: recipient %q not found in the gpg keyring: %s", scheme, o.Recipient, lastGPGLine(out))
		}
	}
	return nil
}

// lastGPGLine picks the meaningful tail of gpg's stderr for an error message,
// dropping the "keybox '…' created" / "directory '…' created" setup noise gpg
// prints on its first run against a fresh GNUPGHOME — so the message ends at the
// actual failure (e.g. "gpg: error reading key: No public key") not the noise.
func lastGPGLine(out []byte) string {
	full := strings.TrimSpace(string(out))
	lines := strings.Split(full, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.Contains(l, "created") {
			continue
		}
		return l
	}
	return full
}

// EncryptCmd returns the encryptor as a pipeline stage, or ok=false for the identity
// (none) scheme. It lets the unified pipeline run encryption through any executor — on
// the client when the key lives there, so plaintext never leaves it.
func EncryptCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return registry.ForwardCmd(norm(scheme), o)
}

// DecryptCmd returns the decryptor as a pipeline stage (the read-side peer of
// EncryptCmd), or ok=false for none. This is the only stage that needs the key.
func DecryptCmd(scheme string, o Options) (cmd programs.Cmd, ok bool, err error) {
	return registry.ReverseCmd(norm(scheme), o)
}

// Filter returns the scheme as a reversible programs.Filter — Forward encrypts, Reverse
// decrypts — for the transform layer to place and chain. The none scheme yields a Filter
// with empty cmds (skipped by the pipeline). It errors only for an unknown scheme.
func Filter(scheme string, o Options) (programs.Filter, error) {
	return registry.Filter(norm(scheme), o)
}
