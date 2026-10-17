package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/biga-sh/server/internal/auth"
	"github.com/biga-sh/server/internal/events"
	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/wire"
)

// Session returns the public view of a session.
func (s *Service) Session(ctx context.Context, id string) (wire.Session, error) {
	row, err := s.store.Q().Session(ctx, id)
	if err != nil {
		return wire.Session{}, sessionErr(err)
	}
	return s.view(row), nil
}

// Authorize checks that token may write to session id. Handlers call it
// before reading a request body, so an unauthorized client cannot make the
// server decompress and parse megabytes; every write re-checks under the
// session lock.
func (s *Service) Authorize(ctx context.Context, id, token string) error {
	row, err := s.store.Q().Session(ctx, id)
	if err != nil {
		return sessionErr(err)
	}
	return checkOwner(row, token)
}

// PutSession creates session id, or reactivates it for its owner. It
// reports whether the session was created, in which case the grant carries
// the new owner token.
func (s *Service) PutSession(ctx context.Context, id, token string, in wire.CreateSessionInput) (wire.SessionGrant, bool, error) {
	var (
		grant   wire.SessionGrant
		created bool
	)
	err := s.store.Tx(ctx, func(q *store.Queries) error {
		newToken, hash := auth.NewToken()
		title := id
		if in.Title != nil {
			title = *in.Title
		}
		row, ok, err := q.InsertSession(ctx, id, title, in.Description, in.GroupID, hash)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		if ok {
			grant, created = wire.SessionGrant{Session: s.view(row), Token: newToken}, true
			return nil
		}

		existing, err := q.LockSession(ctx, id)
		if err != nil {
			return sessionErr(err)
		}
		if err := checkOwner(existing, token); err != nil {
			return err
		}
		row, err = q.ReactivateSession(ctx, id, in.Title, in.Description, in.GroupID)
		if err != nil {
			return fmt.Errorf("reactivate session: %w", err)
		}
		grant = wire.SessionGrant{Session: s.view(row)}
		return nil
	})
	if err != nil {
		return wire.SessionGrant{}, false, err
	}
	grant.ShareURL = s.shareURL(id)
	return grant, created, nil
}

// EndSession ends an active session for its owner and tells its viewers.
func (s *Service) EndSession(ctx context.Context, id, token string) error {
	err := s.store.Tx(ctx, func(q *store.Queries) error {
		row, err := q.LockSession(ctx, id)
		if err != nil {
			return sessionErr(err)
		}
		if err := checkOwner(row, token); err != nil {
			return err
		}
		if row.Status != wire.StatusActive {
			return ErrInactive
		}
		_, err = q.EndSession(ctx, id)
		return sessionErr(err)
	})
	if err != nil {
		return err
	}
	s.publish(ctx, id, "session_ended", struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Timestamp int64  `json:"timestamp"`
	}{"session_ended", id, s.now().UnixMilli()})
	return nil
}

// Heartbeat keeps an active session live for its owner. An ended session
// answers ErrInactive so the CLI reactivates it instead of heartbeating a
// session viewers see as over.
func (s *Service) Heartbeat(ctx context.Context, id, token string) error {
	q := s.store.Q()
	row, err := q.Session(ctx, id)
	if err != nil {
		return sessionErr(err)
	}
	if err := checkOwner(row, token); err != nil {
		return err
	}
	if row.Status != wire.StatusActive {
		return ErrInactive
	}
	return q.TouchSession(ctx, id)
}

// Manifest lists a session's current files for its owner, so a rejoining
// CLI uploads only what changed.
func (s *Service) Manifest(ctx context.Context, id, token string) (wire.Manifest, error) {
	q := s.store.Q()
	row, err := q.Session(ctx, id)
	if err != nil {
		return wire.Manifest{}, sessionErr(err)
	}
	if err := checkOwner(row, token); err != nil {
		return wire.Manifest{}, err
	}
	heads, err := q.HeadFiles(ctx, id)
	if err != nil {
		return wire.Manifest{}, err
	}
	m := wire.Manifest{Files: make([]wire.ManifestEntry, len(heads))}
	for i, h := range heads {
		m.Files[i] = wire.ManifestEntry{Path: h.Path, Hash: h.Hash, Size: h.Size, Binary: h.Binary}
	}
	return m, nil
}

// Stale reports whether a stream for session id should end: the session is
// gone, ended, or silent past the staleness window.
func (s *Service) Stale(ctx context.Context, id string) (bool, error) {
	row, err := s.store.Q().Session(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	return row.Status != wire.StatusActive || s.stale(row), nil
}

// publish sends an event to a session's viewers. Failures are logged:
// the write it reports has already committed.
func (s *Service) publish(ctx context.Context, sessionID, name string, payload any) {
	e, err := events.New(name, payload)
	if err != nil {
		s.log.Error("encode event", "event", name, "error", err)
		return
	}
	s.events.Publish(ctx, sessionID, e)
}

// readBlobs returns the listed blobs, serving what it can from the cache.
func (s *Service) readBlobs(ctx context.Context, q *store.Queries, sess store.SessionRow, hashes []string) (map[string]store.Blob, error) {
	out := make(map[string]store.Blob, len(hashes))
	var misses []string
	for _, h := range hashes {
		if b, ok := s.blobs.get(blobKey(sess.ID, sess.CreatedAt, h)); ok {
			out[h] = b
		} else {
			misses = append(misses, h)
		}
	}
	if len(misses) == 0 {
		return out, nil
	}
	found, err := q.Blobs(ctx, sess.ID, misses)
	if err != nil {
		return nil, fmt.Errorf("read blobs: %w", err)
	}
	for h, b := range found {
		out[h] = b
		s.blobs.put(blobKey(sess.ID, sess.CreatedAt, h), b)
	}
	return out, nil
}

// clampLimit bounds a page size from a query string.
func clampLimit(limit, fallback, ceiling int) int {
	if limit <= 0 {
		return fallback
	}
	return min(limit, ceiling)
}
