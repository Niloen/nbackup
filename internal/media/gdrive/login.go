package gdrive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"

	"github.com/Niloen/nbackup/internal/media"
)

// clientEnv names an OAuth client-secret JSON (a client the operator registers in the
// Google Cloud Console). NBackup ships no shared client — the operator brings their own, so
// no revocable Anthropic/NBackup app sits between them and Google, and the client secret is
// theirs. Overridable per-invocation with `nb login --client`.
const clientEnv = "GOOGLE_OAUTH_CLIENT"

// login is the gdrive Spec.Login hook: `nb login <medium>` for a Drive medium. It runs the
// OAuth consent flow and writes an authorized-user token where the medium reads it, so
// subsequent `nb dump`/`nb sync` runs are unattended. Service-account users skip this.
//
// It picks the flow to fit the client the operator registered (they cannot be told apart
// from the client JSON, so it probes at runtime):
//   - a "TVs and Limited Input devices" client → the RFC 8628 device flow: nb prints a
//     short code + URL, the operator authorizes on any device, nb polls for the token. No
//     browser or open port on this host — the flow for a headless backup server, and the
//     one the gdrive guide recommends.
//   - a "Desktop app" client → a loopback flow: nb binds 127.0.0.1, opens the browser, and
//     captures the redirect itself. No code to copy, no failed-to-load localhost page — but
//     it needs a browser and a free port on THIS host, so it suits a local machine.
//
// Both write the token to a default path (tokenPath) the medium reads back with no env var
// to set — `--out` overrides it. gdrive owns its own flags (parsed from args, after the
// medium name): --client and --out; the neutral `nb login` command names neither.
func login(ctx context.Context, opts media.Options, secretsDir string, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("nb login <gdrive medium>", flag.ContinueOnError)
	fs.SetOutput(out)
	clientFlag := fs.String("client", "", "OAuth client-secret JSON from the Google Cloud Console; or set "+clientEnv)
	outFlag := fs.String("out", "", "where to write the credential token (default: "+tokenPath(secretsDir)+")")
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
  3. create an OAuth client of type "TVs and Limited Input devices" and download its JSON
     (a "Desktop app" client also works, via a browser sign-in on this machine)
then re-run:  nb login <medium> --client <downloaded.json>   (or set %s)

Full walkthrough: the "Backing up to Google Drive" guide (docs/scenarios/gdrive).
On Google Workspace you can skip all of this: share a Shared Drive with a service
account and point %s at its key — no login needed`, clientEnv, credsEnv)
	}
	outPath := *outFlag
	if outPath == "" {
		outPath = tokenPath(secretsDir)
	}

	cb, err := os.ReadFile(clientPath)
	if err != nil {
		return fmt.Errorf("read OAuth client %q: %w", clientPath, err)
	}
	conf, err := clientConfig(cb, drive.DriveFileScope)
	if err != nil {
		return fmt.Errorf("parse OAuth client %q (expected a client-secret JSON downloaded from the Cloud Console): %w", clientPath, err)
	}

	tok, err := authorize(ctx, conf, out)
	if err != nil {
		return err
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
	// 0700 dir / 0600 file: the token is a live credential.
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.WriteFile(outPath, b, 0o600); err != nil {
		return fmt.Errorf("write token %q: %w", outPath, err)
	}
	fmt.Fprintf(out, "\nWrote the token to %s.\n", outPath)
	if outPath == tokenPath(secretsDir) {
		// The default path is exactly where the medium looks, so nothing else to wire.
		fmt.Fprintf(out, "Run `nb check` to confirm the medium opens.\n")
	} else {
		fmt.Fprintf(out, "Set %s=%s (in your shell / cron env) so the medium finds it, then run `nb check`.\n", credsEnv, outPath)
	}
	return nil
}

// clientConfig parses an OAuth client-secret JSON (an "installed" or "web" wrapper) into an
// oauth2.Config. It stands in for google.ConfigFromJSON, which rejects any client that omits
// redirect_uris — but a modern Console download for a "Desktop app" or "TVs and Limited Input
// devices" client carries no redirect_uris at all, and neither flow here needs the file's URI
// anyway (the device flow uses none; the loopback flow mints its own). Missing auth/token URLs
// fall back to Google's well-known endpoints, and the base carries the device endpoint the
// device flow needs.
func clientConfig(jsonKey []byte, scope ...string) (*oauth2.Config, error) {
	type cred struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
	}
	var j struct {
		Web       *cred `json:"web"`
		Installed *cred `json:"installed"`
	}
	if err := json.Unmarshal(jsonKey, &j); err != nil {
		return nil, err
	}
	c := j.Installed
	if c == nil {
		c = j.Web
	}
	if c == nil {
		return nil, errors.New(`no "installed" or "web" client found in the JSON`)
	}
	if c.ClientID == "" {
		return nil, errors.New("client JSON has no client_id")
	}
	endpoint := google.Endpoint // carries Google's DeviceAuthURL for the device flow
	if c.AuthURI != "" {
		endpoint.AuthURL = c.AuthURI
	}
	if c.TokenURI != "" {
		endpoint.TokenURL = c.TokenURI
	}
	conf := &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Scopes:       scope,
		Endpoint:     endpoint,
	}
	if len(c.RedirectURIs) > 0 {
		conf.RedirectURL = c.RedirectURIs[0]
	}
	return conf, nil
}

// authorize runs the OAuth consent, preferring the device flow (no browser or open port on
// this host) and falling back to the loopback browser flow only when the client rejects the
// device endpoint — a "Desktop app" client answers invalid_client, which is the signal to
// switch flows rather than a real failure.
func authorize(ctx context.Context, conf *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	tok, err := deviceFlow(ctx, conf, out)
	if err == nil {
		return tok, nil
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_client" {
		fmt.Fprintln(out, "This OAuth client is a Desktop-app client (no device flow); signing in through the browser instead.")
		return loopbackFlow(ctx, conf, out)
	}
	return nil, err
}

// deviceFlow runs the RFC 8628 device authorization flow: print a short user code and a
// verification URL, then poll the token endpoint until the operator authorizes on another
// device. Fully headless — no browser or listening port on this host.
func deviceFlow(ctx context.Context, conf *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	da, err := conf.DeviceAuth(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, `To authorize NBackup to store backups in Google Drive:

  1. On any device with a browser, open:  %s
  2. Enter this code:  %s

Waiting for you to authorize… (Ctrl-C to cancel)
`, da.VerificationURI, da.UserCode)
	tok, err := conf.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("device authorization (the code may have expired — re-run `nb login`): %w", err)
	}
	return tok, nil
}

// loopbackFlow runs Google's recommended installed-app flow: bind a loopback listener, send
// the operator's browser to Google with a redirect back to it, and capture the code from
// the redirect — no code to copy and no failed-to-load localhost page. It needs a browser
// and a free port on this host, so it is the local-machine counterpart to the device flow.
func loopbackFlow(ctx context.Context, conf *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener for the OAuth redirect: %w", err)
	}
	defer ln.Close()
	conf.RedirectURL = "http://" + ln.Addr().String()

	// A random state ties the redirect to this request (CSRF guard); a mismatch is rejected.
	var sb [16]byte
	if _, err := rand.Read(sb[:]); err != nil {
		return nil, err
	}
	state := hex.EncodeToString(sb[:])

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization denied: "+e, http.StatusBadRequest)
			done <- result{err: fmt.Errorf("authorization denied: %s", e)}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			done <- result{err: errors.New("OAuth state mismatch (the redirect did not match this request)")}
			return
		}
		fmt.Fprintln(w, "NBackup is authorized — you may close this tab and return to the terminal.")
		done <- result{code: q.Get("code")}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	opened := openBrowser(authURL) == nil
	if opened {
		fmt.Fprintf(out, "Opening your browser to authorize NBackup…\nIf it did not open, visit this URL on THIS machine:\n\n  %s\n\n", authURL)
	} else {
		fmt.Fprintf(out, "Visit this URL in a browser on THIS machine to authorize NBackup:\n\n  %s\n\n", authURL)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		if res.code == "" {
			return nil, errors.New("no authorization code in the redirect")
		}
		tok, err := conf.Exchange(ctx, res.code)
		if err != nil {
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		return tok, nil
	}
}

// openBrowser best-effort launches the platform's URL handler. A failure (headless host, no
// handler) is not fatal — the caller prints the URL to open by hand.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return exec.Command(name, args...).Start()
}
