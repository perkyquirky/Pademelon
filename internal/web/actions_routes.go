package web

// actions_routes.go is the HTTP face of internal/actions: submit a verb,
// list the jobs, bulk shutdown. Every route here sits behind requireToken
// AND csrfGuard, and the whole tier only exists when -allow-actions is on —
// a read-only deployment does not just hide these routes, it never
// registers them.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"pademelon/internal/actions"
	"pademelon/internal/clocks"
)

// The proof-of-origin header action POSTs must carry, and the value the
// page sends with it. Named once here so the guard, the tests and the JS
// pin test all reference the same definition instead of copy-pasted
// literals that can drift apart quietly.
const (
	CSRFHeaderName  = "X-Requested-With"
	CSRFHeaderValue = "pademelon"
)

// csrfGuard is the second lock on the action routes. The session cookie is
// SameSite=Lax, which already keeps it off cross-site POSTs; requiring a
// custom header as well means a drive-by needs a CORS preflight, and
// Pademelon never answers preflights. The page's fetches set the header;
// nothing else sends it.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CSRFHeaderName) == "" {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "missing "+CSRFHeaderName+" header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleSubmitAction validates the verb and hands the request to the job
// store. It never talks to libvirt itself: by the time this returns, the
// job is queued (or refused) and the runner owns the rest.
func (s *Server) handleSubmitAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action, err := actions.ParseAction(r.PathValue("action"))
	if err != nil {
		// An unknown verb is a wrong address, not a bad request body.
		http.NotFound(w, r)
		return
	}

	job, err := s.actions.Submit(name, action)
	switch {
	case err == nil:
	case errors.Is(err, actions.ErrUnknownDomain):
		http.NotFound(w, r)
		return
	case errors.Is(err, actions.ErrInvalidState):
		s.actionJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, actions.ErrInFlight):
		// 409 with the job that is already running — the caller can
		// watch that one instead of starting a twin.
		s.actionJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": job})
		return
	case errors.Is(err, actions.ErrMiddlewareOff):
		// The integration is off; the page should not have offered the
		// button, so this is "service unavailable", not a client error.
		s.actionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	default:
		s.log.Error("action submit failed", "domain", name, "action", action, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("action submitted", "domain", name, "action", action, "job", job.ID)
	s.actionJSON(w, http.StatusAccepted, job)
}

// handleRestore is the §8.6 two-path entry point. The body carries the
// mode and the acknowledgement the dialog computed; the store's submit
// validation is the second lock on the dangerous combinations.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	name, snapshot := r.PathValue("name"), r.PathValue("snapshot")
	var body struct {
		Mode       string `json:"mode"`
		StartAfter bool   `json:"startAfter"`
		Ack        bool   `json:"ack"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.actionJSON(w, http.StatusBadRequest, map[string]string{"error": "restore needs a JSON body: {mode, startAfter, ack}"})
		return
	}

	// The id guard may need a fresh middleware fetch, bounded so the
	// submit cannot hang: it waits behind at most one in-flight fetch,
	// then runs its own.
	vctx, cancel := context.WithTimeout(r.Context(), clocks.SnapshotFetchBudget)
	defer cancel()
	job, err := s.actions.SubmitRestore(vctx, name, actions.RestoreOpts{
		SnapshotID: snapshot,
		Mode:       body.Mode,
		StartAfter: body.StartAfter,
		Ack:        body.Ack,
	})
	switch {
	case err == nil:
	case errors.Is(err, actions.ErrGuardUnavailable):
		// The id could not be checked against the middleware — "try
		// again", not "no such snapshot".
		s.actionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, actions.ErrUnknownDomain), errors.Is(err, actions.ErrUnknownSnapshot):
		http.NotFound(w, r)
		return
	case errors.Is(err, actions.ErrBadRestore):
		s.actionJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, actions.ErrInvalidState):
		s.actionJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, actions.ErrInFlight):
		s.actionJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": job})
		return
	case errors.Is(err, actions.ErrMiddlewareOff):
		s.actionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	default:
		s.log.Error("restore submit failed", "domain", name, "snapshot", snapshot, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("restore submitted", "domain", name, "snapshot", snapshot, "mode", body.Mode, "job", job.ID)
	s.actionJSON(w, http.StatusAccepted, job)
}

// handleDelete queues a snapshot deletion. One danger confirm on the
// page, one id guard here, and the audit view keeps the record.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name, snapshot := r.PathValue("name"), r.PathValue("snapshot")

	// Same bounded guard as the restore route.
	vctx, cancel := context.WithTimeout(r.Context(), clocks.SnapshotFetchBudget)
	defer cancel()
	job, err := s.actions.SubmitDelete(vctx, name, snapshot)
	switch {
	case err == nil:
	case errors.Is(err, actions.ErrGuardUnavailable):
		s.actionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, actions.ErrUnknownDomain), errors.Is(err, actions.ErrUnknownSnapshot):
		http.NotFound(w, r)
		return
	case errors.Is(err, actions.ErrInFlight):
		s.actionJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": job})
		return
	case errors.Is(err, actions.ErrMiddlewareOff):
		s.actionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	default:
		s.log.Error("delete submit failed", "domain", name, "snapshot", snapshot, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("delete submitted", "domain", name, "snapshot", snapshot, "job", job.ID)
	s.actionJSON(w, http.StatusAccepted, job)
}

// handleShutdownAll plans the staggered graceful shutdown and returns the
// plan immediately — the staggered submissions happen in the background.
func (s *Server) handleShutdownAll(w http.ResponseWriter, r *http.Request) {
	planned, skipped := s.actions.ShutdownAll()
	s.log.Info("bulk shutdown planned", "planned", planned, "skipped", skipped)
	s.actionJSON(w, http.StatusOK, map[string]any{"planned": planned, "skipped": skipped})
}

// handleJobs is the audit view: every job still in the registry, oldest
// first. Nothing here is authoritative about the guests — the poller is —
// but it answers "what did I just ask for, and did it land?".
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.actionJSON(w, http.StatusOK, map[string]any{"jobs": s.actions.List()})
}

// actionJSON writes the small JSON envelope every action route speaks.
func (s *Server) actionJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
