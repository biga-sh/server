package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SessionRow is one row of the sessions table.
type SessionRow struct {
	ID          string
	Title       string
	Description *string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	EndedAt     *time.Time
	GroupID     *string
	// OwnerTokenHash is nil for sessions created before write protection.
	OwnerTokenHash []byte
	StorageBytes   int64
}

const sessionColumns = `id, title, description, status, created_at, updated_at, ended_at, group_id, owner_token_hash, storage_bytes`

func scanSession(row pgx.Row) (SessionRow, error) {
	var s SessionRow
	err := row.Scan(&s.ID, &s.Title, &s.Description, &s.Status, &s.CreatedAt, &s.UpdatedAt,
		&s.EndedAt, &s.GroupID, &s.OwnerTokenHash, &s.StorageBytes)
	return s, notFound(err)
}

// Session reads one session.
func (q *Queries) Session(ctx context.Context, id string) (SessionRow, error) {
	return scanSession(q.db.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, id))
}

// LockSession reads one session and locks its row until the transaction
// ends, which serializes every write to the session: sequence numbers,
// head files and the storage total are then updated without races.
func (q *Queries) LockSession(ctx context.Context, id string) (SessionRow, error) {
	return scanSession(q.db.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1 FOR UPDATE`, id))
}

// InsertSession creates a session, reporting false (and no error) if one
// with the same ID already exists.
func (q *Queries) InsertSession(ctx context.Context, id, title string, description, groupID *string, tokenHash []byte) (SessionRow, bool, error) {
	s, err := scanSession(q.db.QueryRow(ctx, `
		INSERT INTO sessions (id, title, description, status, group_id, owner_token_hash)
		VALUES ($1, $2, $3, 'active', $4, $5)
		ON CONFLICT (id) DO NOTHING
		RETURNING `+sessionColumns, id, title, description, groupID, tokenHash))
	if errors.Is(err, ErrNotFound) {
		return SessionRow{}, false, nil
	}
	return s, err == nil, err
}

// ReactivateSession marks a session active again, replacing the title,
// description and group only where a new value is given.
func (q *Queries) ReactivateSession(ctx context.Context, id string, title, description, groupID *string) (SessionRow, error) {
	return scanSession(q.db.QueryRow(ctx, `
		UPDATE sessions SET
			title = coalesce($2, title),
			description = coalesce($3, description),
			group_id = coalesce($4, group_id),
			status = 'active',
			updated_at = now(),
			ended_at = NULL
		WHERE id = $1
		RETURNING `+sessionColumns, id, title, description, groupID))
}

// EndSession marks an active session ended. It returns ErrNotFound when the
// session does not exist or has already ended.
func (q *Queries) EndSession(ctx context.Context, id string) (SessionRow, error) {
	return scanSession(q.db.QueryRow(ctx, `
		UPDATE sessions SET status = 'ended', ended_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'active'
		RETURNING `+sessionColumns, id))
}

// TouchSession records activity on a session.
func (q *Queries) TouchSession(ctx context.Context, id string) error {
	_, err := q.db.Exec(ctx, `UPDATE sessions SET updated_at = now() WHERE id = $1`, id)
	return err
}

// AddStorage adds delta bytes to a session's storage total and returns the
// new total.
func (q *Queries) AddStorage(ctx context.Context, id string, delta int64) (int64, error) {
	var total int64
	err := q.db.QueryRow(ctx,
		`UPDATE sessions SET storage_bytes = storage_bytes + $2 WHERE id = $1 RETURNING storage_bytes`,
		id, delta).Scan(&total)
	return total, notFound(err)
}

// MarkAbandoned ends active sessions silent since before cutoff, dating
// their end to their last activity. Their CLI died without saying goodbye.
func (q *Queries) MarkAbandoned(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := q.db.Exec(ctx, `
		UPDATE sessions SET status = 'ended', ended_at = updated_at
		WHERE status = 'active' AND updated_at < $1`, cutoff)
	return tag.RowsAffected(), err
}

// DeleteExpired deletes sessions that ended before cutoff, in batches so
// no single statement holds locks for long. Foreign keys cascade to every
// row the session owns. Protected sessions are excluded inside the
// selection, so they can never stall a batch.
func (q *Queries) DeleteExpired(ctx context.Context, cutoff time.Time, protected []string, batch int) (int64, error) {
	if protected == nil {
		protected = []string{}
	}
	var total int64
	for {
		tag, err := q.db.Exec(ctx, `
			DELETE FROM sessions WHERE id IN (
				SELECT id FROM sessions
				WHERE status = 'ended' AND ended_at < $1 AND NOT (id = ANY($2))
				LIMIT $3
			)`, cutoff, protected, batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
