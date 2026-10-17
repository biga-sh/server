package httpapi

import (
	"context"
	"crypto/subtle"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/wire"
)

// sessionID reads and validates the {id} path segment.
func sessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !wire.ValidSessionID(id) {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid session ID.", nil)
		return "", false
	}
	return id, true
}

// pageParams reads ?limit= and ?before= for history listings.
func pageParams(w http.ResponseWriter, r *http.Request) (limit int, before *int, ok bool) {
	q := r.URL.Query()
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be a positive integer.", nil)
			return 0, nil, false
		}
		limit = n
	}
	if raw := q.Get("before"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > math.MaxInt32 {
			fail(w, r, http.StatusBadRequest, "invalid_request", "before must be a checkpoint number.", nil)
			return 0, nil, false
		}
		before = &n
	}
	return limit, before, true
}

// filePath reads and validates ?path=.
func filePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Missing 'path' query parameter.", nil)
		return "", false
	}
	if err := wire.ValidPath(p); err != nil {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid 'path' query parameter: "+err.Error()+".", nil)
		return "", false
	}
	return p, true
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	s, err := a.svc.Session(r.Context(), id)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, s)
}

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	st, err := a.svc.State(r.Context(), id)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, st)
}

func (a *API) checkpoints(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	limit, before, ok := pageParams(w, r)
	if !ok {
		return
	}
	list, err := a.svc.Checkpoints(r.Context(), id, limit, before)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, map[string]any{"checkpoints": list})
}

func (a *API) checkpointBySeq(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	seq, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil || seq < 0 || seq > math.MaxInt32 {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid checkpoint seq.", nil)
		return
	}
	cp, err := a.svc.Checkpoint(r.Context(), id, seq)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "Checkpoint was not found.")
		return
	}
	// A checkpoint never changes once written.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	respond(w, r, http.StatusOK, cp)
}

func (a *API) file(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	p, ok := filePath(w, r)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "latest"
	}
	if len(ref) > 64 {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid 'ref' query parameter.", nil)
		return
	}
	fc, err := a.svc.FileContent(r.Context(), id, p, ref)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "File version was not found.")
		return
	}
	// Content addressed by hash is immutable; "latest" and checkpoint refs
	// move as the session advances.
	if wire.ValidHash(ref) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	respond(w, r, http.StatusOK, fc)
}

func (a *API) fileHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	p, ok := filePath(w, r)
	if !ok {
		return
	}
	limit, before, ok := pageParams(w, r)
	if !ok {
		return
	}
	history, err := a.svc.FileHistory(r.Context(), id, p, limit, before)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, map[string]any{"history": history})
}

// cron runs the retention sweep for an external scheduler.
func (a *API) cron(w http.ResponseWriter, r *http.Request) {
	token := auth.BearerToken(r)
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.cronSecret)) != 1 {
		fail(w, r, http.StatusUnauthorized, "unauthorized", "Invalid cron credentials.", nil)
		return
	}
	res, err := a.svc.Cleanup(r.Context())
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	respond(w, r, http.StatusOK, res)
}

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if a.ready != nil {
		if err := a.ready(ctx); err != nil {
			a.log.Warn("not ready", "error", err)
			fail(w, r, http.StatusServiceUnavailable, "not_ready", "A dependency is not answering.", nil)
			return
		}
	}
	respond(w, r, http.StatusOK, map[string]string{"status": "ready"})
}
