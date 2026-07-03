package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Niloen/nbackup/internal/config"
)

// httpDoer is the transport seam: http.DefaultClient in production, a recorder in
// tests (so the suite makes no network call).
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// webhookNotifier POSTs an Event as JSON to a webhook URL. The default payload —
// {"<field>": "<subject>\n<body>"} with field defaulting to "text" — is what
// Slack and Discord incoming webhooks accept out of the box; the field is
// configurable for other receivers.
type webhookNotifier struct {
	url     string
	field   string
	headers map[string]string
	client  httpDoer
}

// newWebhook builds a webhook notifier, resolving the URL from the named environment
// variable when url_env is set (preferred for secret endpoints), else the literal
// non-secret url. A configured-but-unset url_env is an error (channel skipped).
func newWebhook(b config.NotifyBackend) (Notifier, error) {
	url := b.URL
	if b.URLEnv != "" {
		url = os.Getenv(b.URLEnv)
		if url == "" {
			return nil, fmt.Errorf("webhook URL env %q is unset or empty", b.URLEnv)
		}
	}
	// url_env/url presence is enforced at load by config's validateNotify.
	field := b.Template
	if field == "" {
		field = "text"
	}
	return &webhookNotifier{url: url, field: field, headers: b.Headers, client: http.DefaultClient}, nil
}

func (n *webhookNotifier) Notify(ctx context.Context, ev Event) error {
	payload := map[string]string{n.field: ev.Subject + "\n\n" + ev.Body}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range n.headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("webhook returned %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}
	return nil
}
