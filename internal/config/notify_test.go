package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNotify(t *testing.T) {
	cases := []struct {
		name    string
		n       NotifyConfig
		wantErr string
	}{
		{
			name: "valid smtp + webhook",
			n: NotifyConfig{
				OnFailure: []string{"mail", "chat"},
				Backends: map[string]NotifyBackend{
					"mail": {Type: "smtp", Host: "mail.x", From: "a@x", To: []string{"ops@x"}, PasswordEnv: "SMTP_PASS"},
					"chat": {Type: "webhook", URLEnv: "SLACK_URL"},
				},
			},
		},
		{
			name:    "unknown type",
			n:       NotifyConfig{Backends: map[string]NotifyBackend{"x": {Type: "carrier-pigeon"}}},
			wantErr: "unknown type",
		},
		{
			name:    "smtp missing recipients",
			n:       NotifyConfig{Backends: map[string]NotifyBackend{"m": {Type: "smtp", Host: "h", From: "a@x"}}},
			wantErr: "requires host, from",
		},
		{
			name:    "webhook missing url",
			n:       NotifyConfig{Backends: map[string]NotifyBackend{"w": {Type: "webhook"}}},
			wantErr: "requires url_env",
		},
		{
			name:    "routing references undefined backend",
			n:       NotifyConfig{OnFailure: []string{"ghost"}, Backends: map[string]NotifyBackend{"m": {Type: "smtp", Host: "h", From: "a@x", To: []string{"o@x"}}}},
			wantErr: "undefined backend",
		},
		{
			name:    "routing with no backends",
			n:       NotifyConfig{Digest: []string{"x"}},
			wantErr: "no backends are defined",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Notify: tc.n}
			err := c.validateNotify()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateNotify: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateNotify err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRejectsLiteralSecret pins the structural secrets stance: a literal
// `password:` under a backend is an unknown field, so KnownFields(true) makes it a
// hard config error rather than a silently-stored secret.
func TestLoadRejectsLiteralSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nbackup.yaml")
	cfg := `
landing: disk
media:
  disk:
    type: disk
    path: /tmp/x
notify:
  on_failure: [mail]
  backends:
    mail:
      type: smtp
      host: mail.x
      from: a@x
      to: [ops@x]
      password: hunter2
sources:
  default:
    app01: [/home]
`
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("Load err = %v, want a rejection of the literal `password` key", err)
	}
}
