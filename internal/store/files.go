package store

import (
	"context"
	"fmt"
	"time"
)

// HeadFile is the current version of one file.
type HeadFile struct {
	Path   string
	Hash   string
	Size   int64
	Binary bool
}

// HeadFiles returns every current file of a session.
func (q *Queries) HeadFiles(ctx context.Context, sessionID string) ([]HeadFile, error) {
	rows, err := q.db.Query(ctx, `SELECT path, hash, size, "binary" FROM files WHERE session_id = $1`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeadFile
	for rows.Next() {
		var f HeadFile
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &f.Binary); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// HeadFilesAt returns the current version of each listed path that exists.
func (q *Queries) HeadFilesAt(ctx context.Context, sessionID string, paths []string) (map[string]HeadFile, error) {
	out := make(map[string]HeadFile, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT path, hash, size, "binary" FROM files WHERE session_id = $1 AND path = ANY($2)`,
		sessionID, paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f HeadFile
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &f.Binary); err != nil {
			return nil, err
		}
		out[f.Path] = f
	}
	return out, rows.Err()
}

// HeadFile returns the current version of one path.
func (q *Queries) HeadFile(ctx context.Context, sessionID, path string) (HeadFile, error) {
	var f HeadFile
	err := q.db.QueryRow(ctx,
		`SELECT path, hash, size, "binary" FROM files WHERE session_id = $1 AND path = $2`,
		sessionID, path).Scan(&f.Path, &f.Hash, &f.Size, &f.Binary)
	return f, notFound(err)
}

// UpsertHeads records new current versions. Paths must be unique.
func (q *Queries) UpsertHeads(ctx context.Context, sessionID string, heads []HeadFile) error {
	if len(heads) == 0 {
		return nil
	}
	paths := make([]string, len(heads))
	hashes := make([]string, len(heads))
	sizes := make([]int32, len(heads))
	binary := make([]bool, len(heads))
	for i, h := range heads {
		paths[i], hashes[i], sizes[i], binary[i] = h.Path, h.Hash, int32(h.Size), h.Binary
	}
	_, err := q.db.Exec(ctx, `
		INSERT INTO files (session_id, path, hash, size, "binary")
		SELECT $1, t.path, t.hash, t.size, t.bin
		FROM unnest($2::text[], $3::text[], $4::int[], $5::bool[]) AS t(path, hash, size, bin)
		ON CONFLICT (session_id, path) DO UPDATE SET
			hash = excluded.hash, size = excluded.size, "binary" = excluded."binary"`,
		sessionID, paths, hashes, sizes, binary)
	if err != nil {
		return fmt.Errorf("upsert head files: %w", err)
	}
	return nil
}

// DeleteHeads removes files that no longer exist.
func (q *Queries) DeleteHeads(ctx context.Context, sessionID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := q.db.Exec(ctx, `DELETE FROM files WHERE session_id = $1 AND path = ANY($2)`, sessionID, paths)
	return err
}

// Stats counts a session's current files and their total size.
func (q *Queries) Stats(ctx context.Context, sessionID string) (files int, bytes int64, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(size), 0) FROM files WHERE session_id = $1`,
		sessionID).Scan(&files, &bytes)
	return files, bytes, err
}

// SnapshotRow is a session's stored tree.
type SnapshotRow struct {
	Tree        []byte
	SequenceNum int64
	Timestamp   time.Time
}

// SaveSnapshot replaces a session's tree. tree is JSON without content.
func (q *Queries) SaveSnapshot(ctx context.Context, sessionID string, tree []byte, sequenceNum int64, ts time.Time) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO snapshots (session_id, tree, sequence_num, "timestamp")
		VALUES ($1, $2::jsonb, $3, $4)
		ON CONFLICT (session_id) DO UPDATE SET
			tree = excluded.tree, sequence_num = excluded.sequence_num, "timestamp" = excluded."timestamp"`,
		sessionID, string(tree), sequenceNum, ts)
	return err
}

// Snapshot reads a session's stored tree.
func (q *Queries) Snapshot(ctx context.Context, sessionID string) (SnapshotRow, error) {
	var s SnapshotRow
	err := q.db.QueryRow(ctx,
		`SELECT tree, sequence_num, "timestamp" FROM snapshots WHERE session_id = $1`,
		sessionID).Scan(&s.Tree, &s.SequenceNum, &s.Timestamp)
	return s, notFound(err)
}
