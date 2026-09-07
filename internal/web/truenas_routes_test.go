package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"pademelon/internal/model"
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
	// The list comes from the cache, never the middleware; an unknown
	// name gets the same 404 as the XML route.
	cache := model.NewCache()
	cache.Set(model.Snapshot{
		Polled:  time.Unix(1788746207, 0),
		Truenas: &model.TruenasGather{Connected: true, Version: "TrueNAS-25.10.5"},
		VMs: []model.VM{{
			Domain: "14_alpine_test",
			Snapshots: []model.ZfsSnapshot{
				{ID: "nvme/vms/x@pademelon-x-2026-09-07_13-08", Name: "pademelon-x-2026-09-07_13-08", Created: 1788749817},
			},
		}},
	})
	s := New(Config{Cache: cache, Log: discardLogger(), Theme: DefaultTheme})

	code, _, body := doGetBody(s, "/api/vm/14_alpine_test/snapshots", nil)
	if code != http.StatusOK {
		t.Fatalf("snapshots route = %d, want 200", code)
	}
	for _, want := range []string{`"id": "nvme/vms/x@pademelon-x-2026-09-07_13-08"`, `"connected": true`} {
		if !strings.Contains(body, want) {
			t.Errorf("snapshots body missing %s, got: %s", want, body)
		}
	}

	code, _, _ = doGetBody(s, "/api/vm/99_ghost/snapshots", nil)
	if code != http.StatusNotFound {
		t.Errorf("unknown domain = %d, want 404", code)
	}

	// Integration off: the route still answers, with truenas null and an
	// empty list — honest "off", not a 404.
	off := New(Config{Cache: model.NewCache(), Log: discardLogger(), Theme: DefaultTheme})
	off.cache.Set(model.Snapshot{VMs: []model.VM{{Domain: "14_alpine_test"}}})
	code, _, body = doGetBody(off, "/api/vm/14_alpine_test/snapshots", nil)
	if code != http.StatusOK || !strings.Contains(body, `"truenas": null`) {
		t.Errorf("integration-off snapshots = %d %s, want 200 with truenas null", code, body)
	}
}
