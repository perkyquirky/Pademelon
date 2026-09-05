package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pademelon/internal/actions"
	"pademelon/internal/model"
)

// fakeActions is the whole ActionSubmitter surface, canned. It lets the
// route tests walk status codes and envelopes without a hypervisor.
type fakeActions struct {
	job        actions.Job
	err        error
	list       []actions.Job
	planned    []string
	skipped    []string
	lastDomain string
	lastAction actions.Action
}

func (f *fakeActions) Submit(domain string, action actions.Action) (*actions.Job, error) {
	f.lastDomain, f.lastAction = domain, action
	if f.err != nil {
		// The real store returns the in-flight job alongside ErrInFlight,
		// so the envelope can carry it; mimic that when one was planted.
		if f.job.ID != "" {
			return &f.job, f.err
		}
		return nil, f.err
	}
	f.job = actions.Job{ID: "abc123", Domain: domain, Action: action, State: actions.StatePending}
	return &f.job, nil
}
func (f *fakeActions) List() []actions.Job { return f.list }
func (f *fakeActions) ShutdownAll() ([]string, []string) {
	return f.planned, f.skipped
}

func newActionsTestServer(token string, submitter ActionSubmitter) *Server {
	return New(Config{
		Cache:   model.NewCache(),
		Log:     discardLogger(),
		Theme:   DefaultTheme,
		Token:   token,
		Actions: submitter,
	})
}

// post sends a POST with the CSRF header and optional cookie, like the
// page's fetches do.
func post(s *Server, path, cookie string, withCSRF bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, nil)
	req.RemoteAddr = "203.0.113.7:4444"
	if withCSRF {
		req.Header.Set("X-Requested-With", "pademelon")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestActionRoutesUnregisteredWithoutActions(t *testing.T) {
	// Nil store: the routes don't exist at all. A read-only deployment is
	// verifiably read-only at runtime, not just by grep. The mux answers a
	// POST to an unregistered path with 405 — the page catch-all only
	// serves GETs — which is the honest "this server doesn't do that".
	s := newActionsTestServer("", nil)

	rec := post(s, "/api/vm/14_alpine_test/shutdown", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("action route with actions disabled = %d, want 405", rec.Code)
	}
	rec = post(s, "/api/actions/shutdown-all", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("shutdown-all with actions disabled = %d, want 405", rec.Code)
	}
}

func TestTypedNilStoreCountsAsDisabled(t *testing.T) {
	// Regression: a nil *Store inside the interface used to register
	// routes that panicked on use — capabilities even claimed actions were
	// on. A typed-nil must count as fully disabled, same as untyped nil.
	var typedNil *actions.Store
	s := New(Config{Cache: model.NewCache(), Log: discardLogger(), Theme: DefaultTheme, Actions: typedNil})

	code, _, body := doGetBody(s, "/api/capabilities", nil)
	if code != http.StatusOK || !strings.Contains(body, `"actions": false`) {
		t.Errorf("capabilities with typed-nil store = %d %s, want actions false", code, body)
	}
	rec := post(s, "/api/vm/14_alpine_test/shutdown", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("action route with typed-nil store = %d, want 405 (unregistered)", rec.Code)
	}
}

func TestActionRoutesRequireToken(t *testing.T) {
	s := newActionsTestServer("tok-123", &fakeActions{})

	rec := post(s, "/api/vm/14_alpine_test/shutdown", "", true)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("action without credentials = %d, want 401", rec.Code)
	}

	// A CSRF-less request must be refused even with a valid token — the
	// header is the second lock, and it costs a cross-site script a CORS
	// preflight we never answer.
	req := httptest.NewRequest("POST", "/api/vm/14_alpine_test/shutdown", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("Authorization", "Bearer tok-123")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("action without CSRF header = %d, want 403", rec.Code)
	}
}

func TestSubmitActionHappyPathAndErrors(t *testing.T) {
	fa := &fakeActions{}
	s := newActionsTestServer("tok-123", fa)
	cookie := loginCookie(t, s, "tok-123")

	rec := post(s, "/api/vm/14_alpine_test/shutdown", cookie, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d, want 202", rec.Code)
	}
	var got actions.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("job decode: %v", err)
	}
	if got.ID != "abc123" || got.Action != actions.ActionShutdown {
		t.Errorf("job = %+v", got)
	}
	if fa.lastDomain != "14_alpine_test" || fa.lastAction != actions.ActionShutdown {
		t.Errorf("submitter saw %s/%s", fa.lastDomain, fa.lastAction)
	}

	// Unknown verb: a wrong address, so 404 — not a 400 that implies the
	// body was read.
	rec = post(s, "/api/vm/14_alpine_test/reboot-the-forest", cookie, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown verb = %d, want 404", rec.Code)
	}

	// Unknown domain and bad state map to 404 / 409 respectively.
	fa.err = actions.ErrUnknownDomain
	rec = post(s, "/api/vm/99_ghost/start", cookie, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown domain = %d, want 404", rec.Code)
	}

	fa.err = actions.ErrInvalidState
	rec = post(s, "/api/vm/14_alpine_test/start", cookie, true)
	if rec.Code != http.StatusConflict {
		t.Errorf("invalid state = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid state") {
		t.Errorf("409 body should explain itself, got: %s", rec.Body.String())
	}

	// In-flight returns 409 *with* the job that's already running, so the
	// caller can watch that one instead of starting a twin.
	fa.err = actions.ErrInFlight
	fa.job = actions.Job{ID: "twin", Domain: "14_alpine_test", Action: actions.ActionShutdown, State: actions.StateRunning}
	rec = post(s, "/api/vm/14_alpine_test/shutdown", cookie, true)
	if rec.Code != http.StatusConflict {
		t.Errorf("in-flight = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id": "twin"`) {
		t.Errorf("409 body should carry the in-flight job, got: %s", rec.Body.String())
	}
}

func TestShutdownAllAndJobsRoutes(t *testing.T) {
	fa := &fakeActions{
		planned: []string{"14_alpine_test"},
		skipped: []string{"12_test (no agent)"},
		list:    []actions.Job{{ID: "abc123", Domain: "14_alpine_test", Action: actions.ActionShutdown, State: actions.StateOK}},
	}
	s := newActionsTestServer("tok-123", fa)
	cookie := loginCookie(t, s, "tok-123")

	rec := post(s, "/api/actions/shutdown-all", cookie, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("shutdown-all = %d, want 200", rec.Code)
	}
	var plan struct {
		Planned []string `json:"planned"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("plan decode: %v", err)
	}
	if len(plan.Planned) != 1 || len(plan.Skipped) != 1 {
		t.Errorf("plan = %+v", plan)
	}

	code, _, body := doGetBody(s, "/api/actions", cookieHeader(cookie))
	if code != http.StatusOK {
		t.Fatalf("jobs = %d, want 200", code)
	}
	if !strings.Contains(body, "abc123") {
		t.Errorf("jobs body missing the job id, got: %s", body)
	}
}

func TestCapabilitiesAdvertiseActions(t *testing.T) {
	on := newActionsTestServer("", &fakeActions{})
	code, _, body := doGetBody(on, "/api/capabilities", nil)
	if code != http.StatusOK || !strings.Contains(body, `"actions": true`) {
		t.Errorf("capabilities with actions on = %d %s, want actions true", code, body)
	}

	off := newActionsTestServer("", nil)
	_, _, body = doGetBody(off, "/api/capabilities", nil)
	if !strings.Contains(body, `"actions": false`) {
		t.Errorf("capabilities with actions off = %s, want actions false", body)
	}
}

// loginCookie walks the Bearer login so tests can hold a session cookie.
func loginCookie(t *testing.T, s *Server, token string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/auth/check", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login issued %d cookies", len(cookies))
	}
	return cookies[0].String()
}

func cookieHeader(cookie string) http.Header {
	h := http.Header{}
	h.Set("Cookie", cookie)
	return h
}
