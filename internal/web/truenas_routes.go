package web

// truenas_routes.go is the read side of the middleware integration: the
// connection status, and the per-VM snapshot list. The client's Run loop
// owns the connection; the snapshot service owns the lists. This file
// only glues them to HTTP.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"pademelon/internal/model"
	"pademelon/internal/truenas"
)

// handleTruenasStatus answers "is the middleware integration on, and is
// its connection up?". It exists even when the integration is off — the
// page needs "off" and "down" to look different, and capabilities alone
// cannot say which one is currently true.
func (s *Server) handleTruenasStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if s.truenas == nil {
		s.writeJSON(w, map[string]any{"enabled": false})
		return
	}
	st := s.truenas.Status()
	s.writeJSON(w, map[string]any{
		"enabled":   true,
		"connected": st.Connected,
		"version":   st.Version,
		"error":     st.Error,
		"since":     st.Since,
	})
}

// handleVMSnapshots serves one VM's zvol snapshot list. This is the one
// read route that can trigger work: the middleware list is too expensive
// to gather on a timer (a dataset with years of auto-* snapshots), so a
// force request starts an on-demand fetch in internal/snapshots and the
// reply says "fetching" until it lands. A non-force request answers from
// what is held. An unknown name gets a 404, same guard as the XML route:
// only what the poller reported is served.
func (s *Server) handleVMSnapshots(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	known := false
	for _, vm := range s.cache.Get().VMs {
		if vm.Domain == name {
			known = true
			break
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.snaps == nil {
		// Integration off: the page needs "off" to look different from
		// "down", so truenas is null rather than a status object.
		s.writeJSON(w, map[string]any{
			"domain":    name,
			"truenas":   nil,
			"snapshots": []model.ZfsSnapshot{},
		})
		return
	}

	force := r.URL.Query().Get("force") == "1"
	st := s.snaps.Request(name, force)
	snaps := st.Snapshots
	if snaps == nil {
		snaps = []model.ZfsSnapshot{}
	}
	s.writeJSON(w, map[string]any{
		"domain":    name,
		"fetched":   st.FetchedAt,
		"fetching":  st.Fetching,
		"stale":     st.Stale,
		"error":     st.Error,
		"truenas":   truenasView(st.Status),
		"snapshots": snaps,
	})
}

// truenasView is the status block the panel renders: the connection state
// at the last fetch attempt, with the reason it failed when it did.
func truenasView(st truenas.Status) map[string]any {
	return map[string]any{
		"connected": st.Connected,
		"version":   st.Version,
	}
}

// handleRefreshSnapshots queues a middleware fetch for every VM, one at a
// time. Like POST /api/refresh it is fire-and-forget: the reply counts
// what was queued, and open panels pick the results up on their ticks.
func (s *Server) handleRefreshSnapshots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.snaps == nil {
		http.Error(w, "middleware integration is off", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "requested %d\n", s.snaps.RefreshAll())
}

// writeJSON is the small encoder the read routes share.
func (s *Server) writeJSON(w http.ResponseWriter, body any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
