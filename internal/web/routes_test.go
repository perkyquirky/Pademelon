package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pademelon/internal/model"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func httptestRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "203.0.113.7:4444"
	return req
}

// TestXMLRouteServesSnapshotCopy checks the happy path: a domain the
// poller reported gets its cached XML back, byte for byte.
func TestXMLRouteServesSnapshotCopy(t *testing.T) {
	cache := model.NewCache()
	cache.Set(model.Snapshot{
		Connected: true,
		VMs: []model.VM{
			{Domain: "7_web", XML: "<domain type='kvm'>\n  <name>7_web</name>\n</domain>"},
			{Domain: "9_no_xml", XML: ""},
		},
	})
	s := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme})

	code, _, body := doGetBody(s, "/api/vm/7_web/xml", nil)
	if code != http.StatusOK {
		t.Fatalf("xml for known domain = %d, want 200", code)
	}
	if !strings.Contains(body, "<name>7_web</name>") {
		t.Errorf("xml body doesn't match the cached copy, got: %q", body)
	}

	// A domain in the snapshot whose XML never landed (poll died mid-way)
	// is a 404, not an empty 200 — a blank file helps nobody.
	if code, _, _ := doGetBody(s, "/api/vm/9_no_xml/xml", nil); code != http.StatusNotFound {
		t.Errorf("xml for snapshot domain with no stored XML = %d, want 404", code)
	}

	// A guessed name the poller never reported is worth nothing.
	if code, _, _ := doGetBody(s, "/api/vm/99_nope/xml", nil); code != http.StatusNotFound {
		t.Errorf("xml for unknown domain = %d, want 404", code)
	}
}

// TestRefreshRoutePokesNudge checks the refresh route pokes the channel
// exactly once when there's room, drops pokes when the buffer is full,
// and doesn't panic when no channel was wired (tests pass nil).
func TestRefreshRoutePokesNudge(t *testing.T) {
	nudge := make(chan struct{}, 1)
	s := New(Config{Cache: model.NewCache(), Log: discardLogger(), Theme: DefaultTheme, Nudge: nudge})

	req := httptestRequest("POST", "/api/refresh")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first refresh = %d, want 200", rec.Code)
	}
	select {
	case <-nudge:
	default:
		t.Fatal("first refresh should have poked the nudge channel")
	}

	// Buffer full: the second poke is dropped, not queued, and still 200.
	// The channel still holds the first poke — that's the debounce working.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second refresh = %d, want 200", rec.Code)
	}
	if len(nudge) != 1 {
		t.Fatalf("nudge channel holds %d pokes, want 1 (second must be dropped)", len(nudge))
	}

	// Wrong method never reaches the handler: the mux sends the GET to the
	// page catch-all, which 404s unknown paths. Either way, no poke.
	reqGet := httptestRequest("GET", "/api/refresh")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, reqGet)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/refresh = %d, want 404", rec.Code)
	}
	if len(nudge) != 1 {
		t.Fatalf("nudge channel holds %d pokes after GET, want 1", len(nudge))
	}

	// No channel wired: still 200, no panic, no poke anywhere.
	sNoNudge := New(Config{Cache: model.NewCache(), Log: discardLogger(), Theme: DefaultTheme})
	rec = httptest.NewRecorder()
	sNoNudge.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh with nil nudge = %d, want 200", rec.Code)
	}
}

// TestPageMarkupSyncWithRoutes keeps the page's fetches pointing at the
// routes that exist, in the spirit of TestAuthMarkupSyncWithPage: the
// expansion panel's XML viewer, the refresh button, the CSRF header and
// the action affordances must never drift away from the server.
func TestPageMarkupSyncWithRoutes(t *testing.T) {
	page := string(indexHTML)

	for _, want := range []string{
		`fetch("/api/refresh"`,
		"/api/vm/",
		`"X-Requested-With": "pademelon"`,
		`/api/vm/${encodeURIComponent(domain)}/`,
		`/api/actions/shutdown-all`,
		`id="btn-refresh"`,
		`id="btn-shutdown-all"`,
		`id="xml-dialog"`,
		`id="confirm-dialog"`,
		`class="vmcell"`,
		`class="detail"`,
		`class="menu-btn"`,
		`caps.actions`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
