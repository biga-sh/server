package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/biga-sh/server/internal/service"
	"github.com/biga-sh/server/internal/wire"
)

// serviceError answers a failed service call. inactiveCode is the error
// code for an ended session, which differs by endpoint for compatibility
// with existing clients.
func (a *API) serviceError(w http.ResponseWriter, r *http.Request, err error, inactiveCode, notFoundMsg string) {
	var verr *wire.ValidationError
	switch {
	case errors.As(err, &verr):
		fail(w, r, http.StatusBadRequest, "invalid_request", "The request is invalid: "+verr.Error(),
			map[string]any{"field": verr.Field, "reason": verr.Reason})
	case errors.Is(err, service.ErrNotFound):
		fail(w, r, http.StatusNotFound, "not_found", "Session was not found.", nil)
	case errors.Is(err, service.ErrNoVersion):
		fail(w, r, http.StatusNotFound, "not_found", notFoundMsg, nil)
	case errors.Is(err, service.ErrInactive):
		fail(w, r, http.StatusConflict, inactiveCode, "Session is not active.", nil)
	case errors.Is(err, service.ErrUnauthorized):
		a.authFailed(r)
		fail(w, r, http.StatusUnauthorized, "unauthorized", "Writing to this session requires its owner token.", nil)
	case errors.Is(err, service.ErrForbidden):
		a.authFailed(r)
		fail(w, r, http.StatusForbidden, "forbidden", "The session token does not match this session.", nil)
	case errors.Is(err, service.ErrLocked):
		a.authFailed(r)
		fail(w, r, http.StatusForbidden, "session_locked",
			"This session was created before owner tokens and can no longer be written to. Start a new session.", nil)
	case errors.Is(err, service.ErrQuota):
		fail(w, r, http.StatusRequestEntityTooLarge, "storage_quota_exceeded",
			"This session has reached its storage limit. Start a new session.", nil)
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client went away; nobody is listening for an answer.
	default:
		a.log.Error("request failed", "request_id", requestID(r.Context()), "path", r.URL.Path, "error", err)
		fail(w, r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred.", nil)
	}
}

// bodyError answers a body that could not be read.
func bodyError(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, errTooLarge):
		fail(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", what+" payload is too large.", nil)
	case errors.Is(err, errUnsupportedEncoding):
		fail(w, r, http.StatusUnsupportedMediaType, "unsupported_encoding", "Unsupported Content-Encoding; send gzip or nothing.", nil)
	case errors.Is(err, errUnsupportedType):
		fail(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.", nil)
	default:
		fail(w, r, http.StatusBadRequest, "invalid_request", "Invalid "+lower(what)+" payload: "+err.Error(), nil)
	}
}

func lower(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if 'A' <= b[0] && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}
