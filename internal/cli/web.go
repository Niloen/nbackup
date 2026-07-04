package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/web"
)

const webLong = `Serve a read-only status website for browsing backup health.

'nb web' starts a small HTTP server that renders the same information as the
'nb run', 'nb medium', 'nb report', and 'nb status' commands — an at-a-glance
dashboard for a browser or phone when you don't have shell access. It is
read-only: it never starts, prunes, or alters anything, and takes no lock, so it
is safe to run alongside a backup.

It binds to 0.0.0.0:8080 by default, reachable from your LAN. There is no
authentication or TLS, so expose it only on a trusted network — or bind it to
127.0.0.1 and front it with a reverse proxy or a VPN (e.g. Tailscale) for
remote access.`

// newWebCmd implements `nb web`: a read-only status HTTP server. It builds a plain
// (unlocked) engine like the other browsing commands and serves the catalog,
// run-history, and live-progress data as HTML — never a write path.
func newWebCmd(a *app) *cobra.Command {
	addr := ":8080"
	cmd := &cobra.Command{
		Use:     "web",
		Short:   "Serve a read-only status website",
		Long:    webLong,
		Args:    cobra.NoArgs,
		Example: "  nb web\n  nb web --addr 127.0.0.1:8080",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadOrDefaultCatalog()
			if err != nil {
				return err
			}
			eng, err := engine.New(cfg)
			if err != nil {
				return err
			}
			srv := web.NewServer(engineSource{eng}, cfg.WorkdirPath())
			return serveWeb(cmd.Context(), addr, srv.Handler())
		},
	}
	cmd.Flags().StringVar(&addr, "addr", addr, "address to listen on (host:port)")
	return cmd
}

// serveWeb runs the HTTP server until the command's context is canceled (Ctrl-C),
// then shuts it down gracefully. Binding is done up front so a port-in-use error is
// reported immediately rather than swallowed by ListenAndServe's goroutine.
func serveWeb(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	fmt.Fprintf(os.Stderr, "nb web: serving read-only status on http://%s  (Ctrl-C to stop)\n", ln.Addr())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// engineSource adapts the engine to web.Source: it promotes the engine's own
// read-only Media/DisplayDLE and forwards the catalog's run/placement reads. This
// adapter is the single, compile-checked list of everything the webui can touch —
// if a page needs a new datum, it surfaces here rather than as an ad-hoc call.
type engineSource struct{ *engine.Engine }

func (e engineSource) Runs() []*catalog.Run { return e.Catalog().Runs() }

func (e engineSource) ReadRun(id string) (*catalog.Run, error) { return e.Catalog().ReadRun(id) }

func (e engineSource) Placements(runID string) []catalog.Placement {
	return e.Catalog().Placements(runID)
}

func (e engineSource) DLESummaries() []catalog.DLESummary { return e.Catalog().DLESummaries() }
