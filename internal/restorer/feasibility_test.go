package restorer

import (
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// TestClientSideKeyRestore covers the server-side-restore feasibility gate: a client-side
// symmetric key is a hard error (no escrow path), a client-side public key is a warning
// (may be escrowed on the server), and server-side / plaintext are clean.
func TestClientSideKeyRestore(t *testing.T) {
	cases := []struct {
		name     string
		ec       config.EncryptConfig
		wantErr  string
		wantWarn bool
	}{
		{
			name:    "client symmetric — infeasible",
			ec:      config.EncryptConfig{Scheme: "gpg", At: "client", PassphraseFile: "/k/pass"},
			wantErr: "passphrase never leaves the client",
		},
		{
			name:     "client public-key — warn (may be escrowed)",
			ec:       config.EncryptConfig{Scheme: "gpg", At: "client", Recipient: "k@x"},
			wantWarn: true,
		},
		{
			name: "server-side encryption — fine",
			ec:   config.EncryptConfig{Scheme: "gpg", At: "server", Recipient: "k@x"},
		},
		{
			name: "server-side default (at unset) — fine",
			ec:   config.EncryptConfig{Scheme: "gpg", Recipient: "k@x"},
		},
		{
			name: "plaintext — fine",
			ec:   config.EncryptConfig{At: "client"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, warn := clientSideKeyRestore(tc.ec, "app01-home")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if warn != tc.wantWarn {
				t.Fatalf("warn = %v, want %v", warn, tc.wantWarn)
			}
		})
	}
}
