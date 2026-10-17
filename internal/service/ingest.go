package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/biga-sh/server/internal/linediff"
	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/wire"
)

// IngestSnapshot replaces a session's tree with an uploaded snapshot and
// records what changed since the previous tree as one checkpoint. snap and
// files come from [wire.ValidateSnapshot].
func (s *Service) IngestSnapshot(ctx context.Context, id, token string, snap *wire.FileSnapshot, files []wire.File) error {
	tree, err := json.Marshal(snap.Tree) // metadata only: MarshalJSON drops content
	if err != nil {
		return fmt.Errorf("encode tree: %w", err)
	}
	blobs := encodeFiles(files)
	now := s.now()

	var cp *wire.Checkpoint
	err = s.store.Tx(ctx, func(q *store.Queries) error {
		sess, err := s.lockForWrite(ctx, q, id, token)
		if err != nil {
			return err
		}
		prior, err := q.HeadFiles(ctx, id)
		if err != nil {
			return err
		}
		maxSeq, err := q.MaxSeq(ctx, id)
		if err != nil {
			return err
		}
		if err := s.storeBlobs(ctx, q, id, blobs); err != nil {
			return err
		}
		if err := q.SaveSnapshot(ctx, id, tree, snap.SequenceNum, now); err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}

		priorByPath := make(map[string]store.HeadFile, len(prior))
		for _, p := range prior {
			priorByPath[p.Path] = p
		}
		incoming := make(map[string]bool, len(files))
		var needed []string
		for _, f := range files {
			incoming[f.Path] = true
			if p, ok := priorByPath[f.Path]; ok && p.Hash != f.Hash && !f.Binary && !p.Binary {
				needed = append(needed, p.Hash)
			}
		}
		for _, p := range prior {
			if !incoming[p.Path] && !p.Binary {
				needed = append(needed, p.Hash)
			}
		}
		priorBlobs, err := s.readBlobs(ctx, q, sess, needed)
		if err != nil {
			return err
		}

		var c changeSet
		for _, f := range files {
			p, existed := priorByPath[f.Path]
			if existed && p.Hash == f.Hash {
				continue
			}
			ch := wire.CheckpointChange{Path: f.Path, ChangeType: wire.Added, AfterHash: new(f.Hash), Binary: f.Binary}
			switch {
			case existed:
				ch.ChangeType, ch.BeforeHash = wire.Modified, new(p.Hash)
				if !f.Binary && !p.Binary {
					ch.Additions, ch.Deletions = linediff.Count(priorBlobs[p.Hash].Content, f.Content)
				}
			case !f.Binary:
				ch.Additions = linediff.Lines(f.Content)
			}
			c.add(ch)
			c.upserts = append(c.upserts, store.HeadFile{Path: f.Path, Hash: f.Hash, Size: f.Size, Binary: f.Binary})
		}
		for _, p := range prior {
			if incoming[p.Path] {
				continue
			}
			c.add(deletion(p, priorBlobs))
			c.deletes = append(c.deletes, p.Path)
		}

		if len(c.changes) > 0 {
			label := "Resumed session"
			if maxSeq < 0 {
				label = "Initial snapshot"
			}
			if cp, err = c.commit(ctx, q, id, maxSeq+1, label, now); err != nil {
				return err
			}
		}
		return q.TouchSession(ctx, id)
	})
	if err != nil {
		return err
	}

	s.publish(ctx, id, "snapshot", struct {
		Type    string            `json:"type"`
		Payload wire.FileSnapshot `json:"payload"`
	}{"snapshot", wire.FileSnapshot{SessionID: id, Tree: snap.Tree, Timestamp: now.UnixMilli(), SequenceNum: snap.SequenceNum}})
	if cp != nil {
		s.publishCheckpoint(ctx, cp)
	}
	return nil
}

// IngestCheckpoint records one batch of changes. up comes from
// [wire.ValidateCheckpoint].
func (s *Service) IngestCheckpoint(ctx context.Context, id, token string, up *wire.CheckpointUpload) error {
	files := make([]wire.File, 0, len(up.Entries))
	paths := make([]string, len(up.Entries))
	for i, e := range up.Entries {
		paths[i] = e.Path
		if e.Hash != nil {
			files = append(files, wire.File{Path: e.Path, Hash: *e.Hash, Size: e.Size, Binary: e.Binary, Content: e.Content})
		}
	}
	blobs := encodeFiles(files)
	now := s.now()

	var cp *wire.Checkpoint
	err := s.store.Tx(ctx, func(q *store.Queries) error {
		sess, err := s.lockForWrite(ctx, q, id, token)
		if err != nil {
			return err
		}
		heads, err := q.HeadFilesAt(ctx, id, paths)
		if err != nil {
			return err
		}
		var needed []string
		for _, e := range up.Entries {
			p, ok := heads[e.Path]
			if !ok || p.Binary {
				continue
			}
			if e.ChangeType == wire.Deleted || (*e.Hash != p.Hash && !e.Binary) {
				needed = append(needed, p.Hash)
			}
		}
		priorBlobs, err := s.readBlobs(ctx, q, sess, needed)
		if err != nil {
			return err
		}

		var c changeSet
		changed := make(map[string]bool)
		for _, e := range up.Entries {
			p, existed := heads[e.Path]
			if e.ChangeType == wire.Deleted {
				if existed { // deleting a file the server never had is a no-op
					c.add(deletion(p, priorBlobs))
					c.deletes = append(c.deletes, e.Path)
				}
				continue
			}
			if existed && p.Hash == *e.Hash {
				continue
			}
			ch := wire.CheckpointChange{Path: e.Path, ChangeType: wire.Added, OldPath: e.OldPath, AfterHash: e.Hash, Binary: e.Binary}
			switch {
			case existed:
				ch.ChangeType, ch.BeforeHash = wire.Modified, new(p.Hash)
				if !e.Binary && !p.Binary {
					ch.Additions, ch.Deletions = linediff.Count(priorBlobs[p.Hash].Content, e.Content)
				}
			case !e.Binary:
				ch.Additions = linediff.Lines(e.Content)
			}
			c.add(ch)
			c.upserts = append(c.upserts, store.HeadFile{Path: e.Path, Hash: *e.Hash, Size: e.Size, Binary: e.Binary})
			changed[*e.Hash] = true
		}
		if len(c.changes) == 0 {
			return q.TouchSession(ctx, id)
		}

		var fresh []store.EncodedBlob
		for _, b := range blobs {
			if changed[b.Hash] {
				fresh = append(fresh, b)
			}
		}
		if err := s.storeBlobs(ctx, q, id, fresh); err != nil {
			return err
		}
		maxSeq, err := q.MaxSeq(ctx, id)
		if err != nil {
			return err
		}
		if cp, err = c.commit(ctx, q, id, maxSeq+1, label(c.changes), now); err != nil {
			return err
		}
		return q.TouchSession(ctx, id)
	})
	if err != nil {
		return err
	}
	if cp != nil {
		s.publishCheckpoint(ctx, cp)
	}
	return nil
}

// lockForWrite locks the session row for the rest of the transaction and
// checks that token owns it and that it is active.
func (s *Service) lockForWrite(ctx context.Context, q *store.Queries, id, token string) (store.SessionRow, error) {
	sess, err := q.LockSession(ctx, id)
	if err != nil {
		return sess, sessionErr(err)
	}
	if err := checkOwner(sess, token); err != nil {
		return sess, err
	}
	if sess.Status != wire.StatusActive {
		return sess, ErrInactive
	}
	return sess, nil
}

// storeBlobs inserts new blobs and enforces the session's storage quota.
// Exceeding it fails the whole transaction.
func (s *Service) storeBlobs(ctx context.Context, q *store.Queries, id string, blobs []store.EncodedBlob) error {
	if len(blobs) == 0 {
		return nil
	}
	added, err := q.InsertBlobs(ctx, id, blobs)
	if err != nil {
		return err
	}
	total, err := q.AddStorage(ctx, id, added)
	if err != nil {
		return err
	}
	if s.opts.StorageQuota > 0 && total > s.opts.StorageQuota {
		return ErrQuota
	}
	return nil
}

func (s *Service) publishCheckpoint(ctx context.Context, cp *wire.Checkpoint) {
	s.publish(ctx, cp.SessionID, "checkpoint", struct {
		Type    string           `json:"type"`
		Payload *wire.Checkpoint `json:"payload"`
	}{"checkpoint", cp})
}

// encodeFiles compresses each distinct content once, outside the
// transaction.
func encodeFiles(files []wire.File) []store.EncodedBlob {
	seen := make(map[string]bool, len(files))
	in := make([]store.BlobInput, 0, len(files))
	for _, f := range files {
		if seen[f.Hash] {
			continue
		}
		seen[f.Hash] = true
		in = append(in, store.BlobInput{Hash: f.Hash, Content: f.Content, Size: f.Size, Binary: f.Binary})
	}
	return store.EncodeBlobs(in)
}

func deletion(p store.HeadFile, priorBlobs map[string]store.Blob) wire.CheckpointChange {
	ch := wire.CheckpointChange{Path: p.Path, ChangeType: wire.Deleted, BeforeHash: new(p.Hash), Binary: p.Binary}
	if b, ok := priorBlobs[p.Hash]; ok && !p.Binary && !b.Binary {
		ch.Deletions = linediff.Lines(b.Content)
	}
	return ch
}

// changeSet accumulates a checkpoint's changes and the head-file updates
// that go with them.
type changeSet struct {
	changes             []wire.CheckpointChange
	upserts             []store.HeadFile
	deletes             []string
	additions, removals int
}

func (c *changeSet) add(ch wire.CheckpointChange) {
	c.changes = append(c.changes, ch)
	c.additions += ch.Additions
	c.removals += ch.Deletions
}

// commit writes the checkpoint and the new head state.
func (c *changeSet) commit(ctx context.Context, q *store.Queries, id string, seq int, label string, now time.Time) (*wire.Checkpoint, error) {
	row := store.CheckpointRow{
		Seq: seq, Label: label, Additions: c.additions, Deletions: c.removals,
		FilesChanged: len(c.changes), CreatedAt: now,
	}
	if err := q.InsertCheckpoint(ctx, id, row, c.changes); err != nil {
		return nil, err
	}
	if err := q.UpsertHeads(ctx, id, c.upserts); err != nil {
		return nil, err
	}
	if err := q.DeleteHeads(ctx, id, c.deletes); err != nil {
		return nil, err
	}
	return &wire.Checkpoint{
		CheckpointSummary: wire.CheckpointSummary{
			SessionID: id, Seq: seq, Label: label, CreatedAt: wire.Time(now),
			FilesChanged: len(c.changes), Additions: c.additions, Deletions: c.removals,
		},
		Changes: c.changes,
	}, nil
}

var verbs = map[wire.ChangeType]string{
	wire.Added:    "Added",
	wire.Modified: "Updated",
	wire.Deleted:  "Deleted",
	wire.Renamed:  "Renamed",
}

// label names a checkpoint after its first change, e.g. "Updated main.go
// +2 more".
func label(changes []wire.CheckpointChange) string {
	first := changes[0]
	l := verbs[first.ChangeType] + " " + path.Base(first.Path)
	if len(changes) > 1 {
		l += " +" + strconv.Itoa(len(changes)-1) + " more"
	}
	return l
}
