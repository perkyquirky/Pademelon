package web

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pademelon/internal/clocks"
	"pademelon/internal/model"
)

// newTestServer builds a server with or without a token, as the tests need.
func newTestServer(token string) *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(model.NewCache(), log, DefaultTheme, token)
}

func doGet(s *Server, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.7:4444"
	if header != nil {
		req.Header = header
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func doGetBody(s *Server, path string, header http.Header) (int, *http.Response, string) {
	rec := doGet(s, path, header)
	res := rec.Result()
	return res.StatusCode, res, rec.Body.String()
}

// TestAuthDisabledKeepsPublicTier checks that an empty token means the
// pre-auth server: no auth-check route at all, capabilities advertising no
// auth, and everything else reachable as before.
func TestAuthDisabledKeepsPublicTier(t *testing.T) {
	s := newTestServer("")

	code, _, body := doGetBody(s, "/api/capabilities", nil)
	if code != http.StatusOK {
		t.Fatalf("capabilities = %d, want 200", code)
	}
	if !strings.Contains(body, `"authRequired": false`) {
		t.Errorf("capabilities should say authRequired false, got: %s", body)
	}

	code, _, _ = doGetBody(s, "/api/auth/check", nil)
	if code != http.StatusNotFound {
		t.Errorf("auth/check with auth disabled = %d, want 404 (route must not exist)", code)
	}
	code, _, _ = doGetBody(s, "/api/auth/logout", nil)
	if code != http.StatusNotFound {
		t.Errorf("auth/logout with auth disabled = %d, want 404 (route must not exist)", code)
	}

	code, _, _ = doGetBody(s, "/api/vms", nil)
	if code != http.StatusOK {
		t.Errorf("/api/vms with auth disabled = %d, want 200 (reads stay public)", code)
	}
}

// TestAuthEnabledPublicTier checks the allowlist: the page, health probe and
// capabilities stay open; /api/vms stays open (reads are public by design);
// only the private tier demands the token.
func TestAuthEnabledPublicTier(t *testing.T) {
	s := newTestServer("tok-123")

	for _, path := range []string{"/", "/api/vms"} {
		code, _, _ := doGetBody(s, path, nil)
		if code != http.StatusOK {
			t.Errorf("%s without token = %d, want 200 (public tier)", path, code)
		}
	}
	// healthz is public but reflects libvirt state; with an empty test cache
	// it answers 503. What matters is that it is reachable — never 401.
	if code, _, _ := doGetBody(s, "/healthz", nil); code == http.StatusUnauthorized {
		t.Error("/healthz must stay token-exempt (Docker HEALTHCHECK and the self-probe use it)")
	}

	code, _, body := doGetBody(s, "/api/capabilities", nil)
	if code != http.StatusOK {
		t.Fatalf("capabilities = %d, want 200", code)
	}
	for _, want := range []string{`"authRequired": true`, `"actions": false`, `"exec": false`} {
		if !strings.Contains(body, want) {
			t.Errorf("capabilities missing %s, got: %s", want, body)
		}
	}

	code, _, _ = doGetBody(s, "/api/auth/check", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("auth/check without token = %d, want 401", code)
	}
}

// TestBearerLoginIssuesCookie walks the actual login flow: correct Bearer
// token → 200, session cookie set with the right flags, and that cookie
// alone (no header) authenticates the next request.
func TestBearerLoginIssuesCookie(t *testing.T) {
	s := newTestServer("tok-123")

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer tok-123")
	code, res, _ := doGetBody(s, "/api/auth/check", hdr)
	if code != http.StatusOK {
		t.Fatalf("auth/check with correct bearer = %d, want 200", code)
	}

	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one cookie issued, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookieName {
		t.Errorf("cookie name = %q, want %q", c.Name, sessionCookieName)
	}
	if c.Value != "tok-123" {
		t.Error("cookie value is not the token")
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie must be SameSite Lax (the CSRF defense)")
	}
	if c.MaxAge != int(clocks.SessionCookieMaxAge/time.Second) {
		t.Errorf("cookie MaxAge = %d, want %d", c.MaxAge, int(clocks.SessionCookieMaxAge/time.Second))
	}
	if c.Secure {
		t.Error("cookie must not be Secure on a plain-HTTP request (browser would drop it)")
	}

	// The issued cookie authenticates the next request with no header.
	code, _, _ = doGetBody(s, "/api/auth/check", http.Header{"Cookie": []string{c.String()}})
	if code != http.StatusOK {
		t.Errorf("auth/check with session cookie = %d, want 200", code)
	}
}

// TestBearerWrongTokenRejected checks wrong tokens are 401s, and that a
// valid cookie issued by an earlier token dies when the token is rotated.
func TestBearerWrongTokenRejected(t *testing.T) {
	s := newTestServer("tok-123")
	start := time.Now()
	code, _, _ := doGetBody(s, "/api/auth/check", http.Header{"Authorization": []string{"Bearer nope"}})
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", code)
	}
	if elapsed := time.Since(start); elapsed < clocks.AuthBackoffBase-100*time.Millisecond {
		t.Errorf("failed attempt returned after %s; expected at least ~%s of backoff", elapsed, clocks.AuthBackoffBase)
	}

	// A cookie minted under the old token must not survive rotation.
	old := newTestServer("old-token")
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer old-token")
	_, res, _ := doGetBody(old, "/api/auth/check", hdr)
	cookie := res.Cookies()[0]

	rotated := newTestServer("new-token")
	code, _, _ = doGetBody(rotated, "/api/auth/check", http.Header{"Cookie": []string{cookie.String()}})
	if code != http.StatusUnauthorized {
		t.Errorf("cookie from rotated-away token = %d, want 401", code)
	}
}

// TestLogoutClearsCookie walks login → logout: the logout response must
// delete the cookie in the browser (Max-Age 0, value emptied, same
// attributes so the browser matches it), and afterwards the old cookie no
// longer authenticates anything.
func TestLogoutClearsCookie(t *testing.T) {
	s := newTestServer("tok-123")

	// Log in.
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer tok-123")
	code, res, _ := doGetBody(s, "/api/auth/check", hdr)
	if code != http.StatusOK {
		t.Fatalf("login = %d, want 200", code)
	}
	cookie := res.Cookies()[0]

	// Log out with the cookie.
	code, res, _ = doGetBody(s, "/api/auth/logout", http.Header{"Cookie": []string{cookie.String()}})
	if code != http.StatusOK {
		t.Fatalf("logout = %d, want 200", code)
	}
	logout := res.Cookies()
	if len(logout) != 1 {
		t.Fatalf("logout issued %d cookies, want 1", len(logout))
	}
	lc := logout[0]
	if lc.Name != sessionCookieName {
		t.Errorf("logout cookie name = %q, want %q", lc.Name, sessionCookieName)
	}
	if lc.Value != "" {
		t.Errorf("logout cookie value = %q, want empty", lc.Value)
	}
	if lc.MaxAge != -1 {
		t.Errorf("logout cookie MaxAge = %d, want -1 (delete now)", lc.MaxAge)
	}
	if !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode {
		t.Error("logout cookie must mirror the issued cookie's attributes so the browser replaces it")
	}

	// A browser that applied the deletion sends no usable cookie anymore.
	code, _, _ = doGetBody(s, "/api/auth/check", http.Header{"Cookie": []string{lc.String()}})
	if code != http.StatusUnauthorized {
		t.Errorf("auth/check after logout = %d, want 401", code)
	}

	// Logout requires credentials too — you can't log out an anonymous
	// session that doesn't exist.
	code, _, _ = doGetBody(s, "/api/auth/logout", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("logout without credentials = %d, want 401", code)
	}
}

// TestBackoffSequence pins the exponential progression so a refactor can't
// quietly flatten or explode it.
func TestBackoffSequence(t *testing.T) {
	want := []time.Duration{
		500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		if got := backoffFor(i + 1); got != w {
			t.Errorf("backoffFor(%d) = %s, want %s", i+1, got, w)
		}
	}
}

// TestSuccessClearsBackoff checks a correct attempt resets the per-source
// counter, so a human who failed once isn't stuck on escalating delays.
func TestSuccessClearsBackoff(t *testing.T) {
	s := newTestServer("tok-123")
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer tok-123")
	doGet(s, "/api/auth/check", hdr) // success

	s.auth.mu.Lock()
	n := len(s.auth.failures)
	s.auth.mu.Unlock()
	if n != 0 {
		t.Errorf("successful login left %d failure entries, want 0", n)
	}
}

// TestSecureCookieOnTLS checks that an HTTPS request gets a Secure cookie.
func TestSecureCookieOnTLS(t *testing.T) {
	s := newTestServer("tok-123")
	req := httptest.NewRequest("GET", "/api/auth/check", nil)
	req.TLS = &tls.ConnectionState{} // any non-nil TLS state counts as HTTPS
	// (httptest.NewRequest has no RemoteAddr set; the middleware tolerates it.)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("Authorization", "Bearer tok-123")
	rec := httptest.NewRecorder()
	s.requireToken(http.HandlerFunc(s.handleAuthCheck)).ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Error("TLS request should receive a Secure cookie")
	}
}

// TestAuthMarkupSyncWithPage keeps the Go handlers and the page's login UI
// pointing at the same endpoints, the form password-manager friendly, and
// the theme placeholder unique (handleIndex replaces it exactly once).
func TestAuthMarkupSyncWithPage(t *testing.T) {
	page := string(indexHTML)

	for _, want := range []string{
		`fetch("/api/capabilities"`,
		`fetch("/api/auth/check"`,
		`fetch("/api/auth/logout"`,
		`type="password"`,
		`autocomplete="current-password"`,
		`id="btn-auth"`,
		`caps.authRequired`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html missing %q", want)
		}
	}

	// Card view is gone; the masthead must not reference it either.
	if strings.Contains(page, "btn-cards") || strings.Contains(page, "vm-grid") {
		t.Error("index.html still contains card view remnants")
	}

	if n := strings.Count(page, themePlaceholder); n != 1 {
		t.Errorf("themePlaceholder occurs %d times in index.html, want exactly 1", n)
	}
}
