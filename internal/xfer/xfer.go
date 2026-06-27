// Package xfer holds the light, in-process pieces of the backup stream pipeline:
// checksumming and byte counting. The heavy part — compression — is run as an
// external child process (see package compress), so nb stays a thin orchestrator.
package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// HashReader returns the hex sha256 of everything read from r.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
