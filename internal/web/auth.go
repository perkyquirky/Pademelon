package web

// auth.go is the static-token layer from IDEAS-EXPLORED.md §5. One shared
// secret, compared on every request with crypto/subtle; no sessions, no
// accounts, no database. The browser-side story: the login form sends the
// token once as an Authorization: Bearer header, the server responds by
// issuing a session cookie, and from then on the cookie rides along
// automatically on every fetch.
//
// The cookie's value is the token itself. That is deliberate: it makes the
// design stateless (restarts never log anyone out) and makes token rotation
// the revoke switch — change the token and every cookie everywhere is dead
// instantly. HttpOnly keeps the value away from page JavaScript; SameSite
// Lax keeps it off cross-site requests, which is what makes cookies safe
// here where they were the classic CSRF hole.

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"pademelon/internal/clocks"
)

// sessionCookieName is the cookie the browser keeps the token in. It is
// issued on the first successful Bearer-authenticated request and refreshed
// nowhere — its MaxAge runs from login.
const sessionCookieName = "pademelon_session"

// authState holds the private-tier gate. token == "" means auth is off and
// requireToken is never registered; the zero-effort path stays exactly the
// pre-auth server.
type authState struct {
	token string

	mu       sync.Mutex
	failures map[string]*authFailure // keyed by remote IP
}

// authFailure tracks consecutive failed attempts from one source. The count
// drives exponential backoff; resetAt is what pruning uses to forget
// sources that gave up.
type authFailure struct {
	count       int
	resetAt     time.Time
	blockedDown time.Time // failed attempts wait until this before the 401 goes out
}

// backoffFor is the delay after count consecutive failures: base, 2×, 4× …
// capped at AuthBackoffMax. Pure so the sequence is testable without
// sleeping.
func backoffFor(count int) time.Duration {
	d := clocks.AuthBackoffBase << min(count-1, 10)
	if d > clocks.AuthBackoffMax {
		d = clocks.AuthBackoffMax
	}
	return d
}

// throttleFailed records a failed attempt from ip and makes the caller wait
// out an escalating delay before the 401 is sent: base, 2×, 4× … capped at
// AuthBackoffMax. A success clears the counter (clearFailures), so one
// fat-fingered attempt never leaves a human on minutes-long delays. Stale
// entries are pruned as we go so the map cannot grow without bound.
func (a *authState) throttleFailed(ip string, now time.Time) time.Duration {
	a.mu.Lock()
	for key, f := range a.failures {
		if now.After(f.resetAt) {
			delete(a.failures, key)
		}
	}
	f := a.failures[ip]
	if f == nil {
		f = &authFailure{}
		a.failures[ip] = f
	}
	f.count++
	f.resetAt = now.Add(time.Hour)
	d := backoffFor(f.count)
	f.blockedDown = now.Add(d)
	a.mu.Unlock()

	time.Sleep(d)
	return d
}

// clearFailures forgets a source after a success.
func (a *authState) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

// tokenEqual compares candidate against the configured token in constant
// time. Empty-token servers never call this.
func (a *authState) tokenEqual(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
}

// bearerToken extracts the value of an "Authorization: Bearer <token>"
// header, or "" if the header is absent or malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}

// validBearer reports whether the request presented the correct token in
// the Authorization header.
func (s *Server) validBearer(r *http.Request) bool {
	t := bearerToken(r)
	return t != "" && s.auth.tokenEqual(t)
}

// validCookie reports whether the request carried the session cookie with
// the correct value. A token rotation invalidates every issued cookie
// immediately, because this compares against the current token.
func (s *Server) validCookie(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return c.Value != "" && s.auth.tokenEqual(c.Value)
}

// sessionCookie builds the Set-Cookie for a successful Bearer login.
// Secure is only set when the request itself arrived over TLS — on plain
// HTTP it would stop the browser from ever storing the cookie.
func (s *Server) sessionCookie(r *http.Request) *http.Cookie {
	c := &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.auth.token,
		Path:     "/",
		MaxAge:   int(clocks.SessionCookieMaxAge / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if r.TLS != nil {
		c.Secure = true
	}
	return c
}

// requireToken is the private-tier gate. Requests that already prove
// themselves (cookie or Bearer header) pass through; a Bearer request also
// gets the cookie issued, which is how the login form turns a one-off
// header into a persistent browser session. Anything else is throttled and
// rejected.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieOK := s.validCookie(r)
		bearerOK := s.validBearer(r)

		if bearerOK {
			if !cookieOK {
				http.SetCookie(w, s.sessionCookie(r))
				// A fresh login, not just a request riding an existing
				// cookie. Cookie-authenticated requests stay silent — the
				// page fetches every 1.5s and would fill the logs.
				s.log.Info("auth: login succeeded, session cookie issued", "remote", remoteIP(r))
			}
			s.auth.clearFailures(remoteIP(r))
			next.ServeHTTP(w, r)
			return
		}
		if cookieOK {
			next.ServeHTTP(w, r)
			return
		}

		ip := remoteIP(r)
		delay := s.auth.throttleFailed(ip, time.Now())
		s.log.Warn("failed auth attempt", "remote", ip, "path", r.URL.Path, "backoff", delay.Round(time.Millisecond))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// handleCapabilities tells the page what this instance offers, so the UI
// and the server can never disagree (IDEAS-EXPLORED.md §6.2). It is
// deliberately public: the page needs it before it can know whether to
// show the login affordance at all.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]bool{
		"actions":      s.actions != nil,
		"authRequired": s.auth.token != "",
		"exec":         false, // permanently false for now; see §4
	})
}

// handleAuthCheck is the boring endpoint the login form fetches. It exists
// because the read tier is public: without a route that always requires the
// token, the form would have nothing to validate against. The middleware
// has already authenticated the request by the time this runs.
func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("{\"authenticated\":true}\n"))
}

// clearSessionCookie builds the Set-Cookie that deletes the session cookie
// in the browser: same attributes as issued, Max-Age 0, value emptied. The
// server keeps no session state, so logout is purely a browser-side
// clearance — the token itself is unchanged and a stolen token would still
// work; token rotation remains the real revoke switch.
func (s *Server) clearSessionCookie(r *http.Request) *http.Cookie {
	c := s.sessionCookie(r)
	c.Value = ""
	c.MaxAge = -1 // delete now
	return c
}

// handleAuthLogout clears the session cookie, logging the browser out. It
// is a GET to keep the house GET-only rule intact; the worst a cross-site
// top-level navigation could do is log you out, which SameSite=Lax already
// requires a real navigation for. Annoyance-class, not security-class.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.clearSessionCookie(r))
	s.log.Info("auth: logged out", "remote", remoteIP(r))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("{\"loggedOut\":true}\n"))
}

// remoteIP is the backoff key. Behind a reverse proxy every request shares
// the proxy's address, so the throttle there covers all clients at once —
// coarse, but the right granularity for a homelab.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
