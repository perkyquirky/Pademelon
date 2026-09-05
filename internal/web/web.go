// Package web serves the dashboard.
//
// The read tier is GET-only and anonymous: every read handler reads from
// the cache, and nothing a request carries ever reaches libvirt — the
// poller gathers, handlers pour. /api/vm/{name}/xml is the one route with
// a domain name in the URL; it serves the poller's cached copy of the XML
// and 404s for any name the poller didn't report, so the old "domain names
// never come from a URL" rule still holds where it matters. POST
// /api/refresh asks the poll loop for an early poll through a debounced
// channel; the loop, not the request, decides when libvirt is polled.
// A private tier (see auth.go) sits behind a static token; it is only
// registered when a token is configured.
package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"pademelon/internal/actions"
	"pademelon/internal/model"
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
}

// ActionSubmitter is the slice of the actions store the web layer uses.
// An interface, so route handlers test without a hypervisor.
type ActionSubmitter interface {
	Submit(domain string, action actions.Action) (*actions.Job, error)
	List() []actions.Job
	ShutdownAll() (planned []string, skipped []string)
}

// Config is everything New needs. Zero-value fields behave sanely: an
// empty theme falls back to the default, an empty token disables auth, and
// a nil Actions disables every action route.
type Config struct {
	Cache   *model.Cache
	Log     *slog.Logger
	Theme   string
	Token   string
	Nudge   chan<- struct{}
	Actions ActionSubmitter
}

// New returns a Server reading from cache. The theme is the default colour
// theme sent to browsers that haven't picked one themselves; validate it
// with ValidTheme before calling. An empty token disables auth entirely —
// the private tier is not even registered without one. Nudge is the
// channel the refresh route pokes; nil disables the poke. Actions is the
// action job store; nil keeps every action route unregistered, which is
// how a read-only deployment stays verifiably read-only at runtime.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Theme == "" {
		cfg.Theme = DefaultTheme
	}
	// A typed-nil pointer (a nil *Store inside the interface) is the
	// classic Go trap: the interface isn't nil, so the routes register,
	// and calling through them panics. Seen live in production — treat
	// any nil-backed submitter as disabled.
	if cfg.Actions != nil {
		if v := reflect.ValueOf(cfg.Actions); v.Kind() == reflect.Ptr && v.IsNil() {
			cfg.Actions = nil
		}
	}
	return &Server{
		cache:   cfg.Cache,
		log:     cfg.Log,
		theme:   cfg.Theme,
		auth:    authState{token: cfg.Token, failures: make(map[string]*authFailure)},
		nudge:   cfg.Nudge,
		actions: cfg.Actions,
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
	if s.actions != nil {
		mux.Handle("POST /api/vm/{name}/{action}", s.requireToken(csrfGuard(http.HandlerFunc(s.handleSubmitAction))))
		mux.Handle("POST /api/actions/shutdown-all", s.requireToken(csrfGuard(http.HandlerFunc(s.handleShutdownAll))))
		mux.Handle("GET /api/actions", s.requireToken(http.HandlerFunc(s.handleJobs)))
	}
	if s.auth.token != "" {
		mux.Handle("GET /api/auth/check", s.requireToken(http.HandlerFunc(s.handleAuthCheck)))
		mux.Handle("GET /api/auth/logout", s.requireToken(http.HandlerFunc(s.handleAuthLogout)))
	}
	return s.logRequests(mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// GET / is the only path the mux pattern "GET /" won't match exactly, so
	// send anything unknown to a 404 rather than silently serving the page.
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
// request never reaches libvirt, and a domain the poller hasn't reported
// gets a 404. That guard is what keeps a guessed name from being worth
// anything.
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
// wedged guest can never turn this into a slow page load.
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
