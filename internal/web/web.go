// Package web serves the dashboard.
//
// The read tier is GET-only and anonymous: every read handler reads from the
// cache, no handler passes anything from a request through to libvirt, and
// domain names come from the poller, never from a query string. A private
// tier (see auth.go) sits behind a static token; it is only registered when
// a token is configured.
package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

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
	cache *model.Cache
	log   *slog.Logger
	theme string
	auth  authState
}

// New returns a Server reading from cache. theme is the default colour theme
// sent to browsers that haven't picked one themselves; validate it with
// ValidTheme before calling. An empty authToken disables auth entirely —
// the private tier (currently /api/auth/check, later the action routes) is
// not even registered without one.
func New(cache *model.Cache, log *slog.Logger, theme, authToken string) *Server {
	if log == nil {
		log = slog.Default()
	}
	if theme == "" {
		theme = DefaultTheme
	}
	return &Server{
		cache: cache,
		log:   log,
		theme: theme,
		auth:  authState{token: authToken, failures: make(map[string]*authFailure)},
	}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/vms", s.handleVMs)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	if s.auth.token != "" {
		mux.Handle("GET /api/auth/check", s.requireToken(http.HandlerFunc(s.handleAuthCheck)))
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
