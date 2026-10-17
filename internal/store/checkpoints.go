package store

import (
	"context"
	"fmt"
	"time"

	"github.com/biga-sh/server/internal/wire"
)

// CheckpointRow is one checkpoint without its changes.
type CheckpointRow struct {
	Seq          int
	Label        string
	Additions    int
	Deletions    int
	FilesChanged int
	CreatedAt    time.Time
}

// MaxSeq returns the highest checkpoint sequence number, or -1 when the
// session has none.
func (q *Queries) MaxSeq(ctx context.Context, sessionID string) (int, error) {
	var seq int
	err := q.db.QueryRow(ctx,
		`SELECT coalesce(max(seq), -1) FROM checkpoints WHERE session_id = $1`,
		sessionID).Scan(&seq)
	return seq, err
}

// InsertCheckpoint records a checkpoint and its changes.
func (q *Queries) InsertCheckpoint(ctx context.Context, sessionID string, c CheckpointRow, changes []wire.CheckpointChange) error {
	if _, err := q.db.Exec(ctx, `
		INSERT INTO checkpoints (session_id, seq, label, additions, deletions, files_changed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sessionID, c.Seq, c.Label, c.Additions, c.Deletions, c.FilesChanged, c.CreatedAt); err != nil {
		return fmt.Errorf("insert checkpoint: %w", err)
	}
	if len(changes) == 0 {
		return nil
	}

	n := len(changes)
	var (
		paths, types       = make([]string, n), make([]string, n)
		oldPaths           = make([]*string, n)
		befores, afters    = make([]*string, n), make([]*string, n)
		additions, deletes = make([]int32, n), make([]int32, n)
		binary             = make([]bool, n)
	)
	for i, ch := range changes {
		paths[i], types[i] = ch.Path, string(ch.ChangeType)
		if ch.OldPath != "" {
			oldPaths[i] = &ch.OldPath
		}
		befores[i], afters[i] = ch.BeforeHash, ch.AfterHash
		additions[i], deletes[i] = int32(min(ch.Additions, 1<<31-1)), int32(min(ch.Deletions, 1<<31-1))
		binary[i] = ch.Binary
	}
	// WITH ORDINALITY keeps the upload's order in the serial ids, which is
	// the order changes are read back in.
	if _, err := q.db.Exec(ctx, `
		INSERT INTO checkpoint_changes
			(session_id, checkpoint_seq, path, change_type, old_path, before_hash, after_hash, additions, deletions, "binary")
		SELECT $1, $2, t.path, t.kind, t.old_path, t.before_hash, t.after_hash, t.adds, t.dels, t.bin
		FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::int[], $9::int[], $10::bool[])
			WITH ORDINALITY AS t(path, kind, old_path, before_hash, after_hash, adds, dels, bin, ord)
		ORDER BY t.ord`,
		sessionID, c.Seq, paths, types, oldPaths, befores, afters, additions, deletes, binary); err != nil {
		return fmt.Errorf("insert checkpoint changes: %w", err)
	}
	return nil
}

// Checkpoints returns up to limit checkpoints newest first, optionally only
// those before a sequence number.
func (q *Queries) Checkpoints(ctx context.Context, sessionID string, limit int, before *int) ([]CheckpointRow, error) {
	rows, err := q.db.Query(ctx, `
		SELECT seq, label, additions, deletions, files_changed, created_at
		FROM checkpoints
		WHERE session_id = $1 AND ($2::int IS NULL OR seq < $2)
		ORDER BY seq DESC
		LIMIT $3`, sessionID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckpointRow{}
	for rows.Next() {
		var c CheckpointRow
		if err := rows.Scan(&c.Seq, &c.Label, &c.Additions, &c.Deletions, &c.FilesChanged, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Checkpoint returns one checkpoint and its changes in upload order.
func (q *Queries) Checkpoint(ctx context.Context, sessionID string, seq int) (CheckpointRow, []wire.CheckpointChange, error) {
	c := CheckpointRow{Seq: seq}
	err := q.db.QueryRow(ctx, `
		SELECT label, additions, deletions, files_changed, created_at
		FROM checkpoints WHERE session_id = $1 AND seq = $2`, sessionID, seq).
		Scan(&c.Label, &c.Additions, &c.Deletions, &c.FilesChanged, &c.CreatedAt)
	if err != nil {
		return c, nil, notFound(err)
	}
	rows, err := q.db.Query(ctx, `
		SELECT path, change_type, coalesce(old_path, ''), before_hash, after_hash, additions, deletions, "binary"
		FROM checkpoint_changes
		WHERE session_id = $1 AND checkpoint_seq = $2
		ORDER BY id`, sessionID, seq)
	if err != nil {
		return c, nil, err
	}
	defer rows.Close()
	changes := []wire.CheckpointChange{}
	for rows.Next() {
		var ch wire.CheckpointChange
		if err := rows.Scan(&ch.Path, &ch.ChangeType, &ch.OldPath, &ch.BeforeHash, &ch.AfterHash,
			&ch.Additions, &ch.Deletions, &ch.Binary); err != nil {
			return c, nil, err
		}
		changes = append(changes, ch)
	}
	return c, changes, rows.Err()
}

// FileHistory returns up to limit revisions of one path, newest first.
func (q *Queries) FileHistory(ctx context.Context, sessionID, path string, limit int, before *int) ([]wire.FileHistoryEntry, error) {
	rows, err := q.db.Query(ctx, `
		SELECT cc.checkpoint_seq, c.label, c.created_at, cc.change_type, cc.before_hash, cc.after_hash,
			cc.additions, cc.deletions, cc."binary"
		FROM checkpoint_changes cc
		JOIN checkpoints c ON c.session_id = cc.session_id AND c.seq = cc.checkpoint_seq
		WHERE cc.session_id = $1 AND cc.path = $2 AND ($3::int IS NULL OR cc.checkpoint_seq < $3)
		ORDER BY cc.checkpoint_seq DESC
		LIMIT $4`, sessionID, path, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.FileHistoryEntry{}
	for rows.Next() {
		var (
			e  wire.FileHistoryEntry
			at time.Time
		)
		if err := rows.Scan(&e.Seq, &e.Label, &at, &e.ChangeType, &e.BeforeHash, &e.AfterHash,
			&e.Additions, &e.Deletions, &e.Binary); err != nil {
			return nil, err
		}
		e.CreatedAt = wire.Time(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// HashAtSeq returns the content hash path had as of checkpoint seq, or
// ErrNotFound if it did not exist then.
func (q *Queries) HashAtSeq(ctx context.Context, sessionID, path string, seq int) (string, error) {
	var (
		kind string
		hash *string
	)
	err := q.db.QueryRow(ctx, `
		SELECT change_type, after_hash FROM checkpoint_changes
		WHERE session_id = $1 AND path = $2 AND checkpoint_seq <= $3
		ORDER BY checkpoint_seq DESC
		LIMIT 1`, sessionID, path, seq).Scan(&kind, &hash)
	if err != nil {
		return "", notFound(err)
	}
	if kind == string(wire.Deleted) || hash == nil {
		return "", ErrNotFound
	}
	return *hash, nil
}
