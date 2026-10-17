package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/biga-sh/server/internal/store"
	"github.com/biga-sh/server/internal/wire"
)

// Page sizes for history listings.
const (
	DefaultPage = 100
	MaxPage     = 500
	statePage   = 200
)

// State returns everything a viewer needs to open a session: the session,
// its tree, recent checkpoints and, for small sessions, every file's
// content.
func (s *Service) State(ctx context.Context, id string) (*wire.SessionState, error) {
	q := s.store.Q()
	row, err := q.Session(ctx, id)
	if err != nil {
		return nil, sessionErr(err)
	}

	var (
		snap        store.SnapshotRow
		hasSnap     bool
		checkpoints []store.CheckpointRow
		heads       []store.HeadFile
		fileCount   int
		totalBytes  int64
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		snap, err = q.Snapshot(gctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		hasSnap = err == nil
		return err
	})
	g.Go(func() (err error) { checkpoints, err = q.Checkpoints(gctx, id, statePage, nil); return err })
	g.Go(func() (err error) { heads, err = q.HeadFiles(gctx, id); return err })
	g.Go(func() (err error) { fileCount, totalBytes, err = q.Stats(gctx, id); return err })
	if err := g.Wait(); err != nil {
		return nil, err
	}

	state := &wire.SessionState{
		Session:     s.view(row),
		Checkpoints: summaries(id, checkpoints),
		Mode:        wire.ModeLazy,
	}
	if hasSnap {
		tree := json.RawMessage(snap.Tree)
		// The head files are the truth: they include every checkpoint since
		// the snapshot was taken.
		if len(heads) > 0 {
			if tree, err = json.Marshal(treeFromHeads(heads)); err != nil {
				return nil, err
			}
		}
		state.Snapshot = &wire.StoredSnapshot{
			SessionID: id, Tree: tree, Timestamp: snap.Timestamp.UnixMilli(), SequenceNum: snap.SequenceNum,
		}
	}
	if totalBytes <= s.opts.EagerMaxBytes && fileCount <= s.opts.EagerMaxFiles {
		if state.Contents, err = q.HeadContents(ctx, id); err != nil {
			return nil, err
		}
		state.Mode = wire.ModeEager
	}
	return state, nil
}

// Checkpoints returns a page of checkpoint summaries, newest first.
func (s *Service) Checkpoints(ctx context.Context, id string, limit int, before *int) ([]wire.CheckpointSummary, error) {
	q := s.store.Q()
	if _, err := q.Session(ctx, id); err != nil {
		return nil, sessionErr(err)
	}
	rows, err := q.Checkpoints(ctx, id, clampLimit(limit, DefaultPage, MaxPage), before)
	if err != nil {
		return nil, err
	}
	return summaries(id, rows), nil
}

// Checkpoint returns one checkpoint with its changes.
func (s *Service) Checkpoint(ctx context.Context, id string, seq int) (*wire.Checkpoint, error) {
	q := s.store.Q()
	if _, err := q.Session(ctx, id); err != nil {
		return nil, sessionErr(err)
	}
	row, changes, err := q.Checkpoint(ctx, id, seq)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoVersion
	}
	if err != nil {
		return nil, err
	}
	return &wire.Checkpoint{CheckpointSummary: summary(id, row), Changes: changes}, nil
}

// FileHistory returns the revisions of one file, newest first.
func (s *Service) FileHistory(ctx context.Context, id, path string, limit int, before *int) ([]wire.FileHistoryEntry, error) {
	q := s.store.Q()
	if _, err := q.Session(ctx, id); err != nil {
		return nil, sessionErr(err)
	}
	return q.FileHistory(ctx, id, path, clampLimit(limit, DefaultPage, MaxPage), before)
}

// FileContent returns one version of a file. ref is a content hash, the
// word "latest" for the current version, or a checkpoint number for the
// version as of that checkpoint.
func (s *Service) FileContent(ctx context.Context, id, path, ref string) (wire.FileContent, error) {
	q := s.store.Q()
	sess, err := q.Session(ctx, id)
	if err != nil {
		return wire.FileContent{}, sessionErr(err)
	}

	var hash string
	switch {
	case wire.ValidHash(ref):
		hash = strings.ToLower(ref)
	case ref == "latest":
		head, err := q.HeadFile(ctx, id, path)
		if err != nil {
			return wire.FileContent{}, versionErr(err)
		}
		hash = head.Hash
	default:
		seq, err := strconv.Atoi(ref)
		if err != nil || seq < 0 {
			return wire.FileContent{}, ErrNoVersion
		}
		if hash, err = q.HashAtSeq(ctx, id, path, seq); err != nil {
			return wire.FileContent{}, versionErr(err)
		}
	}

	blobs, err := s.readBlobs(ctx, q, sess, []string{hash})
	if err != nil {
		return wire.FileContent{}, err
	}
	b, ok := blobs[hash]
	if !ok {
		return wire.FileContent{}, ErrNoVersion
	}
	return wire.FileContent{Content: b.Content, Binary: b.Binary, Hash: hash}, nil
}

func versionErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoVersion
	}
	return err
}

func summary(id string, c store.CheckpointRow) wire.CheckpointSummary {
	return wire.CheckpointSummary{
		SessionID: id, Seq: c.Seq, Label: c.Label, CreatedAt: wire.Time(c.CreatedAt),
		FilesChanged: c.FilesChanged, Additions: c.Additions, Deletions: c.Deletions,
	}
}

func summaries(id string, rows []store.CheckpointRow) []wire.CheckpointSummary {
	out := make([]wire.CheckpointSummary, len(rows))
	for i, r := range rows {
		out[i] = summary(id, r)
	}
	return out
}

// treeFromHeads builds a sorted tree from flat head files, creating the
// directories their paths imply.
func treeFromHeads(heads []store.HeadFile) *wire.FileNode {
	root := &wire.FileNode{Type: wire.NodeDirectory}
	dirs := map[string]*wire.FileNode{"": root}
	var dirFor func(p string) *wire.FileNode
	dirFor = func(p string) *wire.FileNode {
		if d, ok := dirs[p]; ok {
			return d
		}
		parent, name := "", p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent, name = p[:i], p[i+1:]
		}
		d := &wire.FileNode{Type: wire.NodeDirectory, Name: name, Path: p}
		pd := dirFor(parent)
		pd.Children = append(pd.Children, d)
		dirs[p] = d
		return d
	}
	for _, h := range heads {
		parent, name := "", h.Path
		if i := strings.LastIndexByte(h.Path, '/'); i >= 0 {
			parent, name = h.Path[:i], h.Path[i+1:]
		}
		d := dirFor(parent)
		d.Children = append(d.Children, &wire.FileNode{
			Type: wire.NodeFile, Name: name, Path: h.Path, Size: h.Size, Hash: h.Hash, Binary: h.Binary,
		})
	}
	sortTree(root)
	return root
}

// sortTree orders every directory's children: directories first, then by
// name, case-insensitively. The viewer re-sorts with its own collation, so
// this only needs to be stable and sensible.
func sortTree(n *wire.FileNode) {
	slices.SortFunc(n.Children, func(a, b *wire.FileNode) int {
		if a.Type != b.Type {
			if a.Type == wire.NodeDirectory {
				return -1
			}
			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, c := range n.Children {
		if c.Type == wire.NodeDirectory {
			sortTree(c)
		}
	}
}
