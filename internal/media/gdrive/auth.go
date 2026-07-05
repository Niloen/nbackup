package gdrive

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// credsEnv names the file NBackup authenticates from — the same variable the Google SDKs
// use for a service-account key. Credentials come from the ambient environment, never the
// config (matching the cloud medium), so a config file is safe to commit and share.
const credsEnv = "GOOGLE_APPLICATION_CREDENTIALS"

// openService authenticates and opens a Drive client. The credential file is either a
// service-account key (unattended; a Workspace Shared Drive) or an OAuth authorized-user
// token minted by `nb login <medium>` (a personal or Workspace user's own Drive) —
// google.CredentialsFromJSON parses both from the file's "type" field, so one path serves
// both. The scope is drive.file (non-sensitive: the app sees only files it created), which
// is all a backup medium needs and sidesteps Google's restricted-scope verification.
func openService(ctx context.Context) (*drive.Service, error) {
	pathToCreds := os.Getenv(credsEnv)
	if pathToCreds == "" {
		return nil, fmt.Errorf("gdrive medium needs %s set to a credential file: a service-account key (Workspace Shared Drive), or an OAuth token from `nb login <medium>` (personal Google Drive)", credsEnv)
	}
	b, err := os.ReadFile(pathToCreds)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", credsEnv, pathToCreds, err)
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
