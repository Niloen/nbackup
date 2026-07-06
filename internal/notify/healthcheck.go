package notify

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/config"
)

// healthcheckPingTimeout bounds one ping. It is shorter than dispatchTimeout
// because a healthcheck ping is a bare liveness beacon (no body worth waiting
// long for) and DispatchStart fires it before the covered run's own work even
// starts — a slow ping endpoint must never delay a backup.
const healthcheckPingTimeout = 10 * time.Second

// healthcheckNotifier pings a healthchecks.io-style dead-man's-switch URL:
// "<url>/start" when a covered run begins, "<url>" on success, "<url>/fail" on
// failure. Unlike webhook, this is a liveness signal rather than a report — see
// DispatchRun/DispatchStart for why it bypasses on_failure/on_success routing.
type healthcheckNotifier struct {
	url    string
	client httpDoer
}

// newHealthcheck builds a healthcheck notifier, resolving the URL from the named
// environment variable when url_env is set (preferred — a healthchecks.io/ntfy
// ping URL is itself a bearer credential), else the literal non-secret url.
func newHealthcheck(b config.NotifyBackend) (Notifier, error) {
	url := b.URL
	if b.URLEnv != "" {
		url = os.Getenv(b.URLEnv)
		if url == "" {
			return nil, fmt.Errorf("healthcheck URL env %q is unset or empty", b.URLEnv)
		}
	}
	// url_env/url presence is enforced at load by config's validateNotify.
	return &healthcheckNotifier{url: strings.TrimSuffix(url, "/"), client: http.DefaultClient}, nil
}

// Start pings "<url>/start", marking the covered run as begun.
func (n *healthcheckNotifier) Start(ctx context.Context) error {
	return n.ping(ctx, n.url+"/start")
}

// Notify pings "<url>/fail" on a failed run, else "<url>" (success).
func (n *healthcheckNotifier) Notify(ctx context.Context, ev Event) error {
	url := n.url
	if ev.Failed {
		url += "/fail"
	}
	return n.ping(ctx, url)
}

func (n *healthcheckNotifier) ping(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthcheckPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck ping %s: %s", url, resp.Status)
	}
	return nil
}
