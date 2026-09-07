package web

// truenas_routes.go is the read side of the middleware integration: one
// public route telling the page what the background client's connection
// is doing. The client's Run loop owns the connection — this handler only
// reads its status, exactly like every other handler reads its cache.

import (
	"encoding/json"
	"net/http"

	"pademelon/internal/model"
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

// handleVMSnapshots serves one VM's zvol snapshots from the cache — the
// poll loop already gathered them, so a request never reaches the
// middleware. An unknown name gets a 404, same guard as the XML route:
// only what the poller reported is served.
func (s *Server) handleVMSnapshots(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap := s.cache.Get()
	for _, vm := range snap.VMs {
		if vm.Domain != name {
			continue
		}
		snapshots := vm.Snapshots
		if snapshots == nil {
			snapshots = []model.ZfsSnapshot{}
		}
		s.writeJSON(w, map[string]any{
			"domain":    vm.Domain,
			"polled":    snap.Polled,
			"truenas":   snap.Truenas,
			"snapshots": snapshots,
		})
		return
	}
	http.NotFound(w, r)
}

// writeJSON is the small encoder the read routes share.
func (s *Server) writeJSON(w http.ResponseWriter, body any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
