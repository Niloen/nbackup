package gdrive

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"

	"github.com/Niloen/nbackup/internal/media"
)

// clientEnv names an OAuth client-secret JSON (a "Desktop app" client downloaded from the
// Google Cloud Console). NBackup ships no shared client — the operator brings their own, so
// no revocable Anthropic/NBackup app sits between them and Google, and the client secret is
// theirs. Overridable per-invocation with `nb login --client`.
const clientEnv = "GOOGLE_OAUTH_CLIENT"

// login is the gdrive Spec.Login hook: `nb login <medium>` for a Drive medium. It runs the
// OAuth consent flow HEADLESSLY — no browser is opened and no callback server is bound (a
// remote backup host may have neither). It prints the consent URL, the operator authorizes
// on any device, and pastes back the code from the (deliberately non-loading) loopback
// redirect. The result is an authorized-user token JSON written where the medium reads it,
// so subsequent `nb dump`/`nb sync` runs are unattended. Service-account users skip this.
//
// gdrive owns its own flags (parsed from args, after the medium name): --client and --out.
// The neutral `nb login` command names neither.
func login(ctx context.Context, opts media.Options, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("nb login <gdrive medium>", flag.ContinueOnError)
	fs.SetOutput(out)
	clientFlag := fs.String("client", "", "OAuth client-secret JSON (Desktop app) from the Google Cloud Console; or set "+clientEnv)
	outFlag := fs.String("out", "", "where to write the credential token (default: $"+credsEnv+")")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil // fs already printed usage
		}
		return err
	}

	clientPath := *clientFlag
	if clientPath == "" {
		clientPath = os.Getenv(clientEnv)
	}
	if clientPath == "" {
		// This is the one moment the "you must register your own OAuth client"
		// requirement becomes real, so spell out the whole Console chore rather than
		// leave the user with a bare "client-secret JSON not found". NBackup ships no
		// shared client on purpose (no shared quota, no verification burden), so there is
		// no zero-setup path here — but a Workspace user can sidestep it entirely with a
		// service-account key (no login at all).
		return fmt.Errorf(`gdrive login needs an OAuth client that you register once — NBackup ships none (so there is no shared app or quota). In the Google Cloud Console:
  1. create a project and enable the Google Drive API
  2. configure the OAuth consent screen (User type: External) and Publish it — the
     drive.file scope is non-sensitive, so there is no verification review, and
     publishing stops the token expiring after 7 days
  3. create an OAuth client of type "Desktop app" and download its JSON
then re-run:  nb login <medium> --client <downloaded.json>   (or set %s)

Full walkthrough: the "Backing up to Google Drive" guide (docs/scenarios/gdrive).
On Google Workspace you can skip all of this: share a Shared Drive with a service
account and point %s at its key — no login needed`, clientEnv, credsEnv)
	}
	outPath := *outFlag
	if outPath == "" {
		outPath = os.Getenv(credsEnv)
	}
	if outPath == "" {
		return fmt.Errorf("gdrive login needs an output path for the token: pass `--out <file>` or set %s", credsEnv)
	}

	cb, err := os.ReadFile(clientPath)
	if err != nil {
		return fmt.Errorf("read OAuth client %q: %w", clientPath, err)
	}
	conf, err := google.ConfigFromJSON(cb, drive.DriveFileScope)
	if err != nil {
		return fmt.Errorf("parse OAuth client %q (expected a Desktop-app client-secret JSON): %w", clientPath, err)
	}
	// A loopback redirect the operator's browser will try — and fail — to open; the code is
	// visible in its address bar. This is the headless-safe replacement for the retired OOB
	// flow: no server binds the port, the operator just copies the code back.
	conf.RedirectURL = "http://localhost"

	authURL := conf.AuthCodeURL("state", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Fprintf(out, `To authorize NBackup to store backups in Google Drive:

  1. On any device with a browser, open:

     %s

  2. Sign in and grant access. Your browser will then try to open a
     "http://localhost/?code=...&scope=..." page that WILL NOT LOAD — that is expected.
  3. Copy the "code" value from that address bar (or paste the whole URL below).

Authorization code: `, authURL)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read authorization code: %w", err)
	}
	code := extractCode(strings.TrimSpace(line))
	if code == "" {
		return fmt.Errorf("no authorization code entered")
	}

	tok, err := conf.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchange authorization code (it may be mistyped or expired — re-run `nb login`): %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("Google returned no refresh token; re-run `nb login` (offline access must be granted — revoke the app under your Google account if it was approved before)")
	}

	// The authorized_user shape google.CredentialsFromJSON reads back at medium-open time.
	b, err := json.MarshalIndent(struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}{"authorized_user", conf.ClientID, conf.ClientSecret, tok.RefreshToken}, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the token is a live credential.
	if err := os.WriteFile(outPath, b, 0o600); err != nil {
		return fmt.Errorf("write token %q: %w", outPath, err)
	}
	fmt.Fprintf(out, "\n\nWrote the token to %s.\nEnsure %s=%s is set (in your shell / cron env), then run `nb check` to confirm the medium opens.\n", outPath, credsEnv, outPath)
	return nil
}

// extractCode accepts either a bare authorization code or the full loopback redirect URL
// the operator may paste, returning the code in both cases.
func extractCode(s string) string {
	if strings.Contains(s, "code=") {
		if u, err := url.Parse(s); err == nil {
			if c := u.Query().Get("code"); c != "" {
				return c
			}
		}
	}
	return s
}
