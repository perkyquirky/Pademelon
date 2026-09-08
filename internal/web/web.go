// Package web serves the dashboard.
//
// The read tier is GET-only and anonymous: every read handler reads from
// the cache, and nothing a request carries ever reaches libvirt — the
// poller gathers, handlers pour. /api/vm/{name}/xml is the one route with
// a domain name in the URL; it serves the poller's cached copy of the XML
// and 404s for any name the poller did not report, so the old "domain
// names never come from a URL" rule still holds where it matters. POST
// /api/refresh asks the poll loop for an early poll through a debounced
// channel; the loop, not the request, decides when libvirt is polled.
//
// The middleware snapshot list is the deliberate exception: it is far too
// expensive to gather on a timer, so /api/vm/{name}/snapshots?force=1
// starts a on-demand fetch (internal/snapshots) and POST
// /api/refresh-snapshots sweeps every VM, one at a time. The handler
// itself still never blocks on the middleware — it returns what is held
// and says "fetching" until the fetch lands.
//
// A private tier (see auth.go) sits behind a static token; it is only
// registered when a token is configured.
package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"pademelon/internal/actions"
	"pademelon/internal/model"
	"pademelon/internal/snapshots"
	"pademelon/internal/truenas"
)

//go:embed index.html
var indexHTML []byte

// themePlaceholder is the attribute value in index.html's <html> tag that
// gets swapped for the configured default theme. It is quoted, which CSS
// selectors in the page deliberately are not, so this string matches the
// tag and nothing else.
const themePlaceholder = `data-theme="` + DefaultTheme + `"`

// Server wires the cache to HTTP.
type Server struct {
	cache   *model.Cache
	log     *slog.Logger
	theme   string
	auth    authState
	nudge   chan<- struct{}
	actions ActionSubmitter
	truenas TruenasStatusProvider
	snaps   SnapshotProvider
	snapRF  time.Duration // the -snapshot-auto-refresh interval, 0 = off
}

// ActionSubmitter is the slice of the actions store the web layer uses.
// An interface, so route handlers test without a hypervisor. The context
// bounds the restore/delete id check, never the job.
type ActionSubmitter interface {
	Submit(domain string, action actions.Action) (*actions.Job, error)
	List() []actions.Job
	ShutdownAll() (planned []string, skipped []string)
	SubmitRestore(ctx context.Context, domain string, opts actions.RestoreOpts) (*actions.Job, error)
	SubmitDelete(ctx context.Context, domain, snapshotID string) (*actions.Job, error)
}

// TruenasStatusProvider is the slice of the middleware client the web
// layer uses: its status, and nothing else. Nil means the integration is
// off, which the capabilities endpoint advertises and the UI believes.
type TruenasStatusProvider interface {
	Status() truenas.Status
}

// SnapshotProvider is the slice of the snapshot service the web layer
// uses: on-demand requests for one VM's list, and the refresh-everything
// sweep. Nil means the middleware integration is off. This is the one
// deliberate exception to "handlers never trigger work": a snapshot
// request starts a middleware fetch, because the list is far too
// expensive to gather on a timer (README2, "TrueNAS middleware").
type SnapshotProvider interface {
	Request(domain string, force bool) snapshots.State
	RefreshAll() int
}

// Config is everything New needs. Zero-value fields have safe defaults:
// an empty theme falls back to the default, an empty token disables auth,
// a nil Actions disables every action route, a nil Truenas means the
// middleware integration is off, and a zero SnapshotAutoRefresh disables
// the open-panel auto refresh.
type Config struct {
	Cache   *model.Cache
	Log     *slog.Logger
	Theme   string
	Token   string
	Nudge   chan<- struct{}
	Actions ActionSubmitter
	Truenas TruenasStatusProvider
	Snaps   SnapshotProvider

	// SnapshotAutoRefresh is how often an open panel refetches its
	// snapshot list, advertised to the page via capabilities. Zero
	// disables the timer; the panel still fetches on open and on the
	// refresh button.
	SnapshotAutoRefresh time.Duration
}

// New returns a Server reading from cache. The theme is the default
// colour theme sent to browsers that have not picked one themselves;
// validate it with ValidTheme before calling. An empty token disables
// auth entirely — the private tier is not even registered without one.
// Nudge is the channel the refresh route pokes; nil disables the poke.
// Actions is the action job store; nil keeps every action route
// unregistered, which is how a read-only deployment stays verifiably
// read-only at runtime.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Theme == "" {
		cfg.Theme = DefaultTheme
	}
	// A typed-nil pointer (a nil *Store inside the interface) is the
	// classic Go trap: the interface is not nil, so the routes register,
	// and calling through them panics. Seen live in production — treat
	// any nil-backed submitter as disabled. The same guard covers the
	// middleware client, whose main-side variable is a *truenas.Client.
	if cfg.Actions != nil {
		if v := reflect.ValueOf(cfg.Actions); v.Kind() == reflect.Ptr && v.IsNil() {
			cfg.Actions = nil
		}
	}
	if cfg.Truenas != nil {
		if v := reflect.ValueOf(cfg.Truenas); v.Kind() == reflect.Ptr && v.IsNil() {
			cfg.Truenas = nil
		}
	}
	if cfg.Snaps != nil {
		if v := reflect.ValueOf(cfg.Snaps); v.Kind() == reflect.Ptr && v.IsNil() {
			cfg.Snaps = nil
		}
	}
	return &Server{
		cache:   cfg.Cache,
		log:     cfg.Log,
		theme:   cfg.Theme,
		auth:    authState{token: cfg.Token, failures: make(map[string]*authFailure)},
		nudge:   cfg.Nudge,
		actions: cfg.Actions,
		truenas: cfg.Truenas,
		snaps:   cfg.Snaps,
		snapRF:  cfg.SnapshotAutoRefresh,
	}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/vms", s.handleVMs)
	mux.HandleFunc("GET /api/vm/{name}/xml", s.handleVMXML)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /api/truenas", s.handleTruenasStatus)
	mux.HandleFunc("GET /api/vm/{name}/snapshots", s.handleVMSnapshots)
	mux.HandleFunc("POST /api/refresh-snapshots", s.handleRefreshSnapshots)
	if s.actions != nil {
		mux.Handle("POST /api/vm/{name}/{action}", s.requireToken(csrfGuard(http.HandlerFunc(s.handleSubmitAction))))
		mux.Handle("POST /api/actions/shutdown-all", s.requireToken(csrfGuard(http.HandlerFunc(s.handleShutdownAll))))
		mux.Handle("GET /api/actions", s.requireToken(http.HandlerFunc(s.handleJobs)))
		mux.Handle("POST /api/vm/{name}/snapshot/{snapshot}/restore", s.requireToken(csrfGuard(http.HandlerFunc(s.handleRestore))))
		mux.Handle("DELETE /api/vm/{name}/snapshot/{snapshot}", s.requireToken(csrfGuard(http.HandlerFunc(s.handleDelete))))
	}
	if s.auth.token != "" {
		mux.Handle("GET /api/auth/check", s.requireToken(http.HandlerFunc(s.handleAuthCheck)))
		mux.Handle("GET /api/auth/logout", s.requireToken(http.HandlerFunc(s.handleAuthLogout)))
	}
	return s.logRequests(mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// GET / is the only path the mux pattern "GET /" will not match
	// exactly, so send anything unknown to a 404 rather than silently
	// serving the page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(bytes.ReplaceAll(indexHTML,
		[]byte(themePlaceholder),
		[]byte(`data-theme="`+s.theme+`"`)))
}

func (s *Server) handleVMs(w http.ResponseWriter, r *http.Request) {
	snap := s.cache.Get()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		s.log.Error("encode snapshot", "err", err)
	}
}

// handleVMXML serves the raw domain XML for one VM. The XML comes straight
// from the cache — the poller already fetched it on its last round — so a
// request never reaches libvirt, and a domain the poller has not reported
// gets a 404. A guessed name gets nothing.
func (s *Server) handleVMXML(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, vm := range s.cache.Get().VMs {
		if vm.Domain != name {
			continue
		}
		if vm.XML == "" {
			break
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(vm.XML))
		return
	}
	http.NotFound(w, r)
}

// handleRefresh pokes the poll loop for an out-of-band poll. It is a
// debounced nudge, not a command: the channel holds one slot, the poll
// loop drops nudges that arrive too soon after the previous poll, and a
// stuck guest can never turn this into a slow page load.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.nudge != nil {
		select {
		case s.nudge <- struct{}{}:
			_, _ = w.Write([]byte("nudged\n"))
			return
		default:
			_, _ = w.Write([]byte("already nudged\n"))
			return
		}
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.cache.Get()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !snap.Connected {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("libvirt disconnected\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// logRequests logs at debug only — an auto-refreshing page would otherwise
// write a line every few seconds forever.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"took", time.Since(start).Round(time.Millisecond),
		)
	})
}
