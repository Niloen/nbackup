package gdrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// credsEnv names the file NBackup authenticates from — the same variable the Google SDKs
// use for a service-account key. When it is set it wins, so an operator can point at a
// service-account key or a custom token location; when it is unset a personal-Drive medium
// falls back to the token `nb login` wrote to its default path (tokenPath). Credentials
// never live in the config (matching the cloud medium), so a config file is safe to commit
// and share — the config names only the path convention, never a secret.
const credsEnv = "GOOGLE_APPLICATION_CREDENTIALS"

// tokenPath is the default credential file `nb login` writes and openService reads back
// when credsEnv is unset, under the pool-side secretsDir (config.SecretsPath()). An OAuth
// token authorizes a whole Google account (not a folder), so the natural granularity is one
// token per deployment — a single file, not one keyed per medium. A deployment that
// genuinely needs two accounts points credsEnv (or `nb login --out`) at distinct files.
func tokenPath(secretsDir string) string {
	return filepath.Join(secretsDir, "gdrive.json")
}

// openService authenticates and opens a Drive client. The credential file is either a
// service-account key (unattended; a Workspace Shared Drive) or an OAuth authorized-user
// token minted by `nb login <medium>` (a personal or Workspace user's own Drive) —
// google.CredentialsFromJSON parses both from the file's "type" field, so one path serves
// both. Its location is credsEnv when set, else the default tokenPath under secretsDir. The
// scope is drive.file (non-sensitive: the app sees only files it created), which is all a
// backup medium needs and sidesteps Google's restricted-scope verification.
func openService(ctx context.Context, secretsDir string) (*drive.Service, error) {
	pathToCreds := os.Getenv(credsEnv)
	fromDefault := false
	if pathToCreds == "" {
		pathToCreds, fromDefault = tokenPath(secretsDir), true
	}
	b, err := os.ReadFile(pathToCreds)
	if err != nil {
		// The default path missing means the medium was never logged in — point at the
		// fix rather than leak a bare open error. A set-but-broken credsEnv keeps its
		// own error (the operator chose that path and needs to see it).
		if fromDefault && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("gdrive medium has no credentials: run `nb login <medium>` (personal Google Drive), or set %s to a service-account key (Workspace Shared Drive)", credsEnv)
		}
		return nil, fmt.Errorf("read gdrive credentials %q: %w", pathToCreds, err)
	}
	creds, err := google.CredentialsFromJSON(ctx, b, drive.DriveFileScope)
	if err != nil {
		return nil, fmt.Errorf("parse gdrive credentials %q (expected a service-account or authorized-user JSON): %w", pathToCreds, err)
	}
	svc, err := drive.NewService(ctx, option.WithTokenSource(creds.TokenSource))
	if err != nil {
		return nil, fmt.Errorf("open Google Drive service: %w", err)
	}
	return svc, nil
}
