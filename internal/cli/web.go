package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/catalog"
	"github.com/Niloen/nbackup/internal/config"
	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/planner"
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
remote access.

--reload is a development convenience: the server watches its own executable and
re-execs itself when the binary is replaced on disk, so you can iterate by just
copying a fresh 'nb' over the old one (use an atomic replace — 'install nb DEST'
or 'cp nb DEST.new && mv DEST.new DEST' — not an in-place 'cp', which the kernel
refuses with "text file busy" while the binary runs). Off by default; leave it
off in production.`

// newWebCmd implements `nb web`: a read-only status HTTP server. It builds a plain
// (unlocked) engine like the other browsing commands and serves the catalog,
// run-history, and live-progress data as HTML — never a write path.
func newWebCmd(a *app) *cobra.Command {
	addr := ":8080"
	reload := false
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
			// Watch the config file too, so an edit to a medium's capacity, a new
			// DLE, etc. reaches the browser like a fresh catalog does. Skip it under
			// --catalog, where the config file is ignored entirely (nothing to reload).
			cfgPath := a.cfgPath
			if a.catalog != "" {
				cfgPath = ""
			}
			src, err := newEngineSource(cfg, cfgPath, a.loadOrDefaultCatalog)
			if err != nil {
				return err
			}
			srv := web.NewServer(src, cfg.WorkdirPath())
			return serveWeb(cmd.Context(), addr, srv.Handler(), reload)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", addr, "address to listen on (host:port)")
	cmd.Flags().BoolVar(&reload, "reload", reload, "dev: re-exec when the nb binary is replaced on disk")
	return cmd
}

// serveWeb runs the HTTP server until the command's context is canceled (Ctrl-C),
// then shuts it down gracefully. Binding is done up front so a port-in-use error is
// reported immediately rather than swallowed by ListenAndServe's goroutine.
//
// With reload set, it also watches its own executable and, when the binary is
// replaced on disk, shuts down and re-execs itself so the running server picks up
// the new build (see watchExecutable / reexec). This is a dev convenience, off by
// default.
func serveWeb(ctx context.Context, addr string, h http.Handler, reload bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	fmt.Fprintf(os.Stderr, "nb web: serving read-only status on http://%s  (Ctrl-C to stop)\n", ln.Addr())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	var reloadCh <-chan struct{} // nil unless --reload: a nil channel blocks forever in the select
	if reload {
		reloadCh = watchExecutable(ctx)
	}

	select {
	case <-ctx.Done():
		return srv.Shutdown(shutdownContext())
	case <-reloadCh:
		_ = srv.Shutdown(shutdownContext()) // release the listener before the new image rebinds
		return reexec()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func shutdownContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel // the process exits (Shutdown returns, then re-exec/return); leak is bounded to that instant
	return ctx
}

// watchExecutable polls this process's own binary once a second and closes the
// returned channel the first time its on-disk identity changes. It uses the same
// stat-stamp trick as the catalog watcher, so an atomic replace (rename over the
// path) is seen as a new (mod,size); an in-place write would be too, but the kernel
// forbids that on a running binary. A zero stamp (a transient stat error) is
// ignored so a torn read never triggers a spurious reload.
func watchExecutable(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	exe, err := os.Executable()
	if err != nil {
		return ch // can't resolve our own path: never fires, behaves like reload off
	}
	start := statFile(exe)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s := statFile(exe); s != start && s != (catalogStamp{}) {
					close(ch)
					return
				}
			}
		}
	}()
	return ch
}

// reexec replaces the current process image with a fresh exec of the (now updated)
// binary, preserving argv and environment. On success it does not return.
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "nb web: binary changed, reloading %s\n", exe)
	return syscall.Exec(exe, os.Args, os.Environ())
}

// engineSource adapts the engine to web.Source: it promotes the engine's own
// read-only Media/DisplayDLE and forwards the catalog's run/placement reads. This
// adapter is the single, compile-checked list of everything the webui can touch —
// if a page needs a new datum, it surfaces here rather than as an ad-hoc call.
//
// The engine loads the catalog cache once at construction, but `nb web` is a
// long-running reader beside cron-driven writers (dump/sync/prune run in their own
// processes and rewrite catalog.json). So the source stats the cache file per read
// and rebuilds the engine when it has changed — the browser then always sees the
// current catalog, like the run history and live progress, which are re-read from
// their workdir files per request already. Rebuilding is cheap: engine construction
// is pure wiring plus that one JSON read (the landing volume opens lazily, and no
// web page touches it).
//
// The config file gets the same treatment: an operator editing a medium's capacity,
// adding a DLE, or changing the cycle expects the change to show up without bouncing
// the server. The source stats the config file per read too and, when it changes,
// reloads the whole config (via reload) before rebuilding the engine from it.
type engineSource struct {
	cfg *config.Config

	// cfgPath is the config file to watch, and reload re-reads it into a fresh
	// *config.Config. cfgPath is empty (and reload unused) when there is no config
	// file to watch, e.g. under --catalog.
	cfgPath string
	reload  func() (*config.Config, error)

	mu       sync.Mutex
	eng      *engine.Engine
	stamp    catalogStamp // identity of catalog.json when eng was built
	cfgStamp catalogStamp // identity of the config file when cfg was loaded
}

// catalogStamp identifies a catalog cache file's on-disk version (zero when the
// file is absent — the empty-catalog case).
type catalogStamp struct {
	mod  time.Time
	size int64
}

func statCatalog(workdir string) catalogStamp {
	return statFile(filepath.Join(workdir, catalog.CacheFile))
}

// statFile stamps a file's on-disk identity (zero when it can't be stat'd).
func statFile(path string) catalogStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return catalogStamp{}
	}
	return catalogStamp{mod: fi.ModTime(), size: fi.Size()}
}

// newEngineSource builds the source with its first engine, so `nb web` still fails
// loudly at startup on a broken config or catalog. cfgPath/reload watch the config
// file for live edits; pass cfgPath == "" (reload unused) to disable that watch.
func newEngineSource(cfg *config.Config, cfgPath string, reload func() (*config.Config, error)) (*engineSource, error) {
	stamp := statCatalog(cfg.WorkdirPath())
	eng, err := engine.New(cfg)
	if err != nil {
		return nil, err
	}
	return &engineSource{
		cfg:      cfg,
		cfgPath:  cfgPath,
		reload:   reload,
		eng:      eng,
		stamp:    stamp,
		cfgStamp: statFile(cfgPath),
	}, nil
}

// engine returns the current engine, rebuilding it first when the config file or
// catalog.json has changed on disk since the last build. A config-file change is
// reloaded into a fresh cfg and always forces a rebuild; a bare catalog change
// rebuilds from the existing cfg. A reload or rebuild failure (e.g. a torn read
// racing a writer's rename, or a mid-edit config) keeps serving the previous engine;
// the next request retries.
func (e *engineSource) engine() *engine.Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	rebuild := false
	if e.cfgPath != "" {
		if cs := statFile(e.cfgPath); cs != e.cfgStamp {
			if cfg, err := e.reload(); err == nil {
				e.cfg, e.cfgStamp, rebuild = cfg, cs, true
			}
		}
	}
	if stamp := statCatalog(e.cfg.WorkdirPath()); rebuild || stamp != e.stamp {
		if eng, err := engine.New(e.cfg); err == nil {
			e.eng, e.stamp = eng, stamp
		}
	}
	return e.eng
}

func (e *engineSource) Runs() []*catalog.Run { return e.engine().Catalog().Runs() }

func (e *engineSource) ReadRun(id string) (*catalog.Run, error) {
	return e.engine().Catalog().ReadRun(id)
}

func (e *engineSource) Placements(runID string) []catalog.Placement {
	return e.engine().Catalog().Placements(runID)
}

func (e *engineSource) DLESummaries() []catalog.DLESummary { return e.engine().DLESummaries() }

func (e *engineSource) Media() []engine.MediumInfo { return e.engine().Media() }

func (e *engineSource) MediumStats(name string) (engine.MediumStats, bool) {
	return e.engine().MediumStats(name)
}

func (e *engineSource) MediumProtected(name string, now time.Time) (residual, capacity int64, ok bool) {
	return e.engine().MediumProtected(name, now)
}

func (e *engineSource) RunCoverage(run *catalog.Run) *engine.RunCoverage {
	return e.engine().RunCoverage(run)
}

func (e *engineSource) SyncLags() []engine.SyncLag { return e.engine().SyncLags() }

func (e *engineSource) DisplayDLE(slug string) string { return e.engine().DisplayDLE(slug) }

func (e *engineSource) DLENames() []string { return e.engine().DLENames() }

func (e *engineSource) DrillWindow() time.Duration { return e.cfg.DrillWindow() }

// StaleDLEs reports the engine's overdue set as of now, using the dump cycle as the
// freshness window — always on, no separate config.
func (e *engineSource) StaleDLEs(now time.Time) []catalog.StaleDLE {
	return e.engine().StaleDLEs(e.cfg.CycleDuration(), now)
}

// Forecast projects the next `days` runs OFFLINE (catalog + run-log only, no archiver
// probe), so serving /dles never opens an SSH connection. Errors are advisory — a
// forecast that cannot be built simply yields no ghost cells.
func (e *engineSource) Forecast(start time.Time, days int) []*planner.Plan {
	plans, err := e.engine().SimulateOffline(start, days)
	if err != nil {
		return nil
	}
	return plans
}
