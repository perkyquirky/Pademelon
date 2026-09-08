package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pademelon/internal/model"
	"pademelon/internal/snapshots"
	"pademelon/internal/truenas"
)

// fakeTruenas is the whole TruenasStatusProvider surface, canned.
type fakeTruenas struct {
	st truenas.Status
}

func (f *fakeTruenas) Status() truenas.Status { return f.st }

func newTruenasTestServer(provider TruenasStatusProvider) *Server {
	return New(Config{Cache: model.NewCache(), Log: discardLogger(), Theme: DefaultTheme, Truenas: provider})
}

func TestCapabilitiesAdvertiseTruenas(t *testing.T) {
	on := newTruenasTestServer(&fakeTruenas{})
	_, _, body := doGetBody(on, "/api/capabilities", nil)
	if !strings.Contains(body, `"truenas": true`) {
		t.Errorf("capabilities with middleware on = %s, want truenas true", body)
	}

	off := newTruenasTestServer(nil)
	_, _, body = doGetBody(off, "/api/capabilities", nil)
	if !strings.Contains(body, `"truenas": false`) {
		t.Errorf("capabilities with middleware off = %s, want truenas false", body)
	}
}

func TestTruenasStatusRoute(t *testing.T) {
	// Off: the route exists but says so honestly — the page needs "off"
	// and "down" to look different.
	off := newTruenasTestServer(nil)
	code, _, body := doGetBody(off, "/api/truenas", nil)
	if code != http.StatusOK || !strings.Contains(body, `"enabled": false`) {
		t.Errorf("status with integration off = %d %s, want enabled false", code, body)
	}

	// On and connected: version and connected flag flow through.
	up := newTruenasTestServer(&fakeTruenas{st: truenas.Status{
		Connected: true, Version: "TrueNAS-25.10.5", Since: time.Unix(1788746207, 0),
	}})
	code, _, body = doGetBody(up, "/api/truenas", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, want := range []string{`"connected": true`, `"version": "TrueNAS-25.10.5"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %s, got: %s", want, body)
		}
	}

	// On and down: connected false, error surfaced, version may linger.
	down := newTruenasTestServer(&fakeTruenas{st: truenas.Status{
		Connected: false, Version: "TrueNAS-25.10.5", Error: "dial refused",
	}})
	_, _, body = doGetBody(down, "/api/truenas", nil)
	if !strings.Contains(body, `"connected": false`) || !strings.Contains(body, "dial refused") {
		t.Errorf("down status body = %s, want connected false and the error", body)
	}
	if !strings.Contains(body, "TrueNAS-25.10.5") {
		t.Errorf("down status should keep the last known version, got: %s", body)
	}
}

func TestVMSnapshotsRoute(t *testing.T) {
	// The list comes from the snapshot service; the route only glues it
	// to HTTP. An unknown name gets the same 404 as the XML route.
	cache := model.NewCache()
	cache.Set(model.Snapshot{
		Polled: time.Unix(1788746207, 0),
		VMs:    []model.VM{{Domain: "14_alpine_test"}},
	})
	snaps := &fakeSnaps{out: snapshots.State{
		Domain: "14_alpine_test",
		Status: truenas.Status{Connected: true, Version: "TrueNAS-25.10.5"},
		Snapshots: []model.ZfsSnapshot{
			{ID: "nvme/vms/x@pademelon-x-2026-09-07_13-08", Name: "pademelon-x-2026-09-07_13-08", Created: 1788749817},
		},
	}}
	s := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme, Snaps: snaps})

	code, _, body := doGetBody(s, "/api/vm/14_alpine_test/snapshots", nil)
	if code != http.StatusOK {
		t.Fatalf("snapshots route = %d, want 200", code)
	}
	for _, want := range []string{`"id": "nvme/vms/x@pademelon-x-2026-09-07_13-08"`, `"connected": true`} {
		if !strings.Contains(body, want) {
			t.Errorf("snapshots body missing %s, got: %s", want, body)
		}
	}
	if snaps.requests != 1 || snaps.force {
		t.Errorf("service should see one non-forced request, got %d (force %v)", snaps.requests, snaps.force)
	}

	// A force request reaches the service as one.
	code, _, _ = doGetBody(s, "/api/vm/14_alpine_test/snapshots?force=1", nil)
	if code != http.StatusOK || !snaps.force || snaps.requests != 2 {
		t.Errorf("force request = %d, saw %d requests (last force %v)", code, snaps.requests, snaps.force)
	}

	code, _, _ = doGetBody(s, "/api/vm/99_ghost/snapshots", nil)
	if code != http.StatusNotFound {
		t.Errorf("unknown domain = %d, want 404 (and the service must not see it)", code)
	}
	if snaps.requests != 2 {
		t.Errorf("unknown domain reached the service %d times, want 0 more", snaps.requests-2)
	}

	// Integration off: the route still answers, with truenas null and an
	// empty list — honest "off", not a 404.
	off := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme})
	code, _, body = doGetBody(off, "/api/vm/14_alpine_test/snapshots", nil)
	if code != http.StatusOK || !strings.Contains(body, `"truenas": null`) {
		t.Errorf("integration-off snapshots = %d %s, want 200 with truenas null", code, body)
	}
}

func TestRefreshSnapshotsRoute(t *testing.T) {
	cache := model.NewCache()
	cache.Set(model.Snapshot{VMs: []model.VM{{Domain: "14_alpine_test"}}})
	snaps := &fakeSnaps{}
	s := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme, Snaps: snaps})

	rec := httptest.NewRequest("POST", "/api/refresh-snapshots", nil)
	outer := httptest.NewRecorder()
	s.Handler().ServeHTTP(outer, rec)
	if outer.Code != http.StatusOK || !strings.Contains(outer.Body.String(), "requested 3") {
		t.Errorf("refresh-snapshots = %d %s, want 200 counting the sweep", outer.Code, outer.Body.String())
	}
	if snaps.sweeps != 1 {
		t.Errorf("RefreshAll called %d times, want 1", snaps.sweeps)
	}

	// Integration off: 503, the same honesty as the snapshot verb.
	off := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme})
	outer = httptest.NewRecorder()
	off.Handler().ServeHTTP(outer, rec)
	if outer.Code != http.StatusServiceUnavailable {
		t.Errorf("refresh-snapshots with integration off = %d, want 503", outer.Code)
	}
}

// fakeSnaps is the SnapshotProvider surface, canned.
type fakeSnaps struct {
	out      snapshots.State
	domain   string
	force    bool
	requests int
	sweeps   int
}

func (f *fakeSnaps) Request(domain string, force bool) snapshots.State {
	f.requests++
	f.domain, f.force = domain, force
	return f.out
}

func (f *fakeSnaps) RefreshAll() int {
	f.sweeps++
	return 3
}
