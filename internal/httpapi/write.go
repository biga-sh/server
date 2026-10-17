package httpapi

import (
	"net/http"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/wire"
)

// writeGate runs the checks every write shares, in order of cost: a
// well-formed session ID, the client's auth-failure budget, the access key,
// then the endpoint's rate limit. It reports the session ID and owner token,
// or false after answering the request.
func (a *API) writeGate(w http.ResponseWriter, r *http.Request, limitKey string, lim rateLimit) (id, token string, ok bool) {
	id = r.PathValue("id")
	if !wire.ValidSessionID(id) {
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid session ID.", nil)
		return "", "", false
	}
	client := clientKey(clientAddr(r.Context()))
	if over, wait := a.limiter.Exceeded(r.Context(), "authfail:"+client, limitAuthFailures.n); over {
		retryAfter(w, wait)
		fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many failed authentication attempts. Try again later.", nil)
		return "", "", false
	}
	if a.keys.Required() {
		key := r.Header.Get(auth.KeyHeader)
		if key == "" {
			fail(w, r, http.StatusUnauthorized, "access_key_required",
				"This server only accepts broadcasts from people holding an access key.", nil)
			return "", "", false
		}
		if !a.keys.Valid(key) {
			a.authFailed(r)
			fail(w, r, http.StatusUnauthorized, "access_key_invalid", "The access key is not valid on this server.", nil)
			return "", "", false
		}
	}
	if limitKey == "" {
		limitKey = id
	}
	if ok, wait := a.limiter.Allow(r.Context(), limitKey, lim.n, lim.window); !ok {
		retryAfter(w, wait)
		fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests. Slow down and try again shortly.", nil)
		return "", "", false
	}
	return id, auth.BearerToken(r), true
}

// authFailed counts a failed credential against the client.
func (a *API) authFailed(r *http.Request) {
	key := "authfail:" + clientKey(clientAddr(r.Context()))
	a.limiter.Allow(r.Context(), key, limitAuthFailures.n, limitAuthFailures.window)
}

// owned is writeGate plus a check that the token owns the session, made
// before the body is read.
func (a *API) owned(w http.ResponseWriter, r *http.Request, route string, lim rateLimit) (id, token string, ok bool) {
	id, token, ok = a.writeGate(w, r, route+":"+r.PathValue("id"), lim)
	if !ok {
		return "", "", false
	}
	if err := a.svc.Authorize(r.Context(), id, token); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return "", "", false
	}
	return id, token, true
}

// putSession creates a session, or reactivates it for its owner.
func (a *API) putSession(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.writeGate(w, r, "session:"+clientKey(clientAddr(r.Context())), limitSession)
	if !ok {
		return
	}
	var in wire.CreateSessionInput
	if err := readJSON(w, r, a.limits.SessionBytes, &in, true); err != nil {
		bodyError(w, r, err, "Session")
		return
	}
	if err := wire.ValidateSessionInput(&in); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	grant, created, err := a.svc.PutSession(r.Context(), id, token, in)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respondPrivate(w, r, status, grant)
}

// endSession ends a session.
func (a *API) endSession(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.writeGate(w, r, "end:"+r.PathValue("id"), limitHeartbeat)
	if !ok {
		return
	}
	if err := a.svc.EndSession(r.Context(), id, token); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// snapshot replaces a session's whole tree.
func (a *API) snapshot(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.owned(w, r, "snapshot", limitSnapshot)
	if !ok {
		return
	}
	var snap wire.FileSnapshot
	if err := readJSON(w, r, a.limits.SnapshotBytes, &snap, false); err != nil {
		bodyError(w, r, err, "Snapshot")
		return
	}
	files, err := wire.ValidateSnapshot(&snap, id, wire.TreeLimits{
		MaxFiles: a.limits.SnapshotFiles, MaxFileBytes: a.limits.FileBytes,
	})
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	if err := a.svc.IngestSnapshot(r.Context(), id, token, &snap, files); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	respond(w, r, http.StatusAccepted, map[string]bool{"accepted": true})
}

// checkpoint records one batch of changes.
func (a *API) checkpoint(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.owned(w, r, "checkpoint", limitCheckpoint)
	if !ok {
		return
	}
	var up wire.CheckpointUpload
	if err := readJSON(w, r, a.limits.CheckpointBytes, &up, false); err != nil {
		bodyError(w, r, err, "Checkpoint")
		return
	}
	if err := wire.ValidateCheckpoint(&up, id, a.limits.CheckpointEntries, a.limits.FileBytes); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	if err := a.svc.IngestCheckpoint(r.Context(), id, token, &up); err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	respond(w, r, http.StatusAccepted, map[string]bool{"accepted": true})
}

// heartbeat keeps a broadcast live.
func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.writeGate(w, r, "heartbeat:"+r.PathValue("id"), limitHeartbeat)
	if !ok {
		return
	}
	if err := a.svc.Heartbeat(r.Context(), id, token); err != nil {
		a.serviceError(w, r, err, "inactive_session", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// manifest lists a session's current files for its owner.
func (a *API) manifest(w http.ResponseWriter, r *http.Request) {
	id, token, ok := a.writeGate(w, r, "manifest:"+r.PathValue("id"), limitOwnerRead)
	if !ok {
		return
	}
	m, err := a.svc.Manifest(r.Context(), id, token)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusOK, m)
}
