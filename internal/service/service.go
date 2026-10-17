// Package service holds the server's session logic: who may write to a
// session, how uploads become checkpoints, and what viewers read.
package service

import (
	"errors"
	"log/slog"
	"time"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/events"
	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/wire"
)

// Errors the HTTP layer maps to responses.
var (
	ErrNotFound = errors.New("session not found")
	// ErrNoVersion reports a missing checkpoint or file version in a session
	// that exists.
	ErrNoVersion = errors.New("version not found")
	ErrInactive  = errors.New("session is not active")
	// ErrUnauthorized means the request carried no session token.
	ErrUnauthorized = errors.New("session token required")
	// ErrForbidden means the session token is wrong.
	ErrForbidden = errors.New("session token does not match")
	// ErrLocked means the session predates owner tokens, so nobody can
	// prove they own it.
	ErrLocked = errors.New("session predates owner tokens")
	ErrQuota  = errors.New("session storage quota exceeded")
)

// Options tunes the service.
type Options struct {
	// SessionStale is how long an active session may go silent before
	// viewers see it as ended.
	SessionStale time.Duration
	// EagerMaxBytes and EagerMaxFiles bound sessions whose contents ship
	// with the initial state.
	EagerMaxBytes int64
	EagerMaxFiles int
	// StorageQuota caps one session's stored content, in bytes.
	StorageQuota int64
	// PublicURL is the viewer origin for share links; empty omits them.
	PublicURL string
	// Retention, AbandonAfter and Protected drive the cleanup sweep.
	Retention    time.Duration
	AbandonAfter time.Duration
	Protected    []string
	// BlobCacheBytes bounds the in-process content cache.
	BlobCacheBytes int64
}

// Service implements session operations on top of the store and the event
// hub.
type Service struct {
	store  *store.Store
	events *events.Hub
	blobs  *blobCache
	opts   Options
	log    *slog.Logger
	now    func() time.Time
}

// New returns a service.
func New(st *store.Store, hub *events.Hub, opts Options, log *slog.Logger) *Service {
	return &Service{
		store:  st,
		events: hub,
		blobs:  newBlobCache(opts.BlobCacheBytes),
		opts:   opts,
		log:    log,
		now:    time.Now,
	}
}

// checkOwner decides whether token may write to s.
func checkOwner(s store.SessionRow, token string) error {
	switch {
	case s.OwnerTokenHash == nil:
		return ErrLocked
	case token == "":
		return ErrUnauthorized
	case !auth.CheckToken(token, s.OwnerTokenHash):
		return ErrForbidden
	}
	return nil
}

// stale reports whether an active session has gone silent long enough to
// count as ended.
func (s *Service) stale(r store.SessionRow) bool {
	return r.Status == wire.StatusActive && s.now().Sub(r.UpdatedAt) > s.opts.SessionStale
}

// view is the public form of a session, reporting a silent broadcast as
// ended as of its last activity.
func (s *Service) view(r store.SessionRow) wire.Session {
	w := wire.Session{
		ID:          r.ID,
		Title:       r.Title,
		Description: r.Description,
		Status:      r.Status,
		CreatedAt:   wire.Time(r.CreatedAt),
		UpdatedAt:   wire.Time(r.UpdatedAt),
		EndedAt:     wire.TimePtr(r.EndedAt),
		GroupID:     r.GroupID,
	}
	if s.stale(r) {
		w.Status = wire.StatusEnded
		w.EndedAt = &w.UpdatedAt
	}
	return w
}

func (s *Service) shareURL(id string) string {
	if s.opts.PublicURL == "" {
		return ""
	}
	return s.opts.PublicURL + "/session/" + id
}

func sessionErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
