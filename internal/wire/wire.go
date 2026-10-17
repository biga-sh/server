// Package wire defines the JSON the server exchanges with the CLI and the
// viewer. The shapes match the zod schemas in the website's @biga/types
// package, which remains the reference for the viewer.
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// Session statuses.
const (
	StatusActive = "active"
	StatusEnded  = "ended"
)

// ChangeType describes how a file changed within a checkpoint.
type ChangeType string

// Change types.
const (
	Added    ChangeType = "added"
	Modified ChangeType = "modified"
	Deleted  ChangeType = "deleted"
	Renamed  ChangeType = "renamed"
)

// Valid reports whether c is a known change type.
func (c ChangeType) Valid() bool {
	switch c {
	case Added, Modified, Deleted, Renamed:
		return true
	}
	return false
}

// Session is the public view of a broadcast session.
type Session struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	EndedAt     *string `json:"endedAt"`
	GroupID     *string `json:"groupId"`
}

// SessionGrant answers PUT /api/sessions/:id. Token is the session's owner
// credential and is present only on the response that created the session:
// the server keeps a hash and can never show it again.
type SessionGrant struct {
	Session
	Token    string `json:"token,omitempty"`
	ShareURL string `json:"shareUrl,omitempty"`
}

// CreateSessionInput is the body of PUT /api/sessions/:id.
type CreateSessionInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	GroupID     *string `json:"groupId"`
}

// Node types in a snapshot tree.
const (
	NodeFile      = "file"
	NodeDirectory = "directory"
)

// FileNode is one entry of a snapshot tree. Which fields apply depends on
// Type: files carry Size, Hash, Content and Binary; directories carry
// Children and Ignored.
type FileNode struct {
	Type     string      `json:"type"`
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Size     int64       `json:"size"`
	Hash     string      `json:"hash"`
	Content  string      `json:"content"`
	Binary   bool        `json:"binary"`
	Children []*FileNode `json:"children"`
	Ignored  bool        `json:"ignored"`
}

// MarshalJSON writes the node in its discriminated-union shape, without
// content: stored and served trees are metadata only.
func (n *FileNode) MarshalJSON() ([]byte, error) {
	switch n.Type {
	case NodeFile:
		return json.Marshal(struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			Hash   string `json:"hash"`
			Binary bool   `json:"binary,omitempty"`
		}{n.Type, n.Name, n.Path, n.Size, n.Hash, n.Binary})
	case NodeDirectory:
		children := n.Children
		if children == nil {
			children = []*FileNode{}
		}
		return json.Marshal(struct {
			Type     string      `json:"type"`
			Name     string      `json:"name"`
			Path     string      `json:"path"`
			Children []*FileNode `json:"children"`
			Ignored  bool        `json:"ignored,omitempty"`
		}{n.Type, n.Name, n.Path, children, n.Ignored})
	default:
		return nil, fmt.Errorf("wire: unknown node type %q", n.Type)
	}
}

// FileSnapshot is the full tree, uploaded when a broadcast starts and
// stored (without content) as the session's current tree.
type FileSnapshot struct {
	SessionID string    `json:"sessionId"`
	Tree      *FileNode `json:"tree"`
	// Timestamp is in Unix milliseconds.
	Timestamp   int64 `json:"timestamp"`
	SequenceNum int64 `json:"sequenceNum"`
}

// StoredSnapshot is a snapshot as served back: Tree is pre-encoded JSON.
type StoredSnapshot struct {
	SessionID   string          `json:"sessionId"`
	Tree        json.RawMessage `json:"tree"`
	Timestamp   int64           `json:"timestamp"`
	SequenceNum int64           `json:"sequenceNum"`
}

// CheckpointEntry is one changed file in a checkpoint upload.
type CheckpointEntry struct {
	Path       string     `json:"path"`
	ChangeType ChangeType `json:"changeType"`
	OldPath    string     `json:"oldPath"`
	Hash       *string    `json:"hash"`
	Size       int64      `json:"size"`
	Binary     bool       `json:"binary"`
	Content    string     `json:"content"`
}

// CheckpointUpload is one save burst's worth of changes.
type CheckpointUpload struct {
	SessionID string            `json:"sessionId"`
	Entries   []CheckpointEntry `json:"entries"`
	Timestamp int64             `json:"timestamp"`
}

// CheckpointChange is how one file changed within a checkpoint. Content is
// referenced by hash, never inlined.
type CheckpointChange struct {
	Path       string     `json:"path"`
	ChangeType ChangeType `json:"changeType"`
	OldPath    string     `json:"oldPath,omitempty"`
	BeforeHash *string    `json:"beforeHash"`
	AfterHash  *string    `json:"afterHash"`
	Additions  int        `json:"additions"`
	Deletions  int        `json:"deletions"`
	Binary     bool       `json:"binary"`
}

// CheckpointSummary is a checkpoint without its change list.
type CheckpointSummary struct {
	SessionID    string `json:"sessionId"`
	Seq          int    `json:"seq"`
	Label        string `json:"label"`
	CreatedAt    string `json:"createdAt"`
	FilesChanged int    `json:"filesChanged"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
}

// Checkpoint is one save burst with every file it changed.
type Checkpoint struct {
	CheckpointSummary
	Changes []CheckpointChange `json:"changes"`
}

// FileHistoryEntry is one revision of a single file.
type FileHistoryEntry struct {
	Seq        int        `json:"seq"`
	Label      string     `json:"label"`
	CreatedAt  string     `json:"createdAt"`
	ChangeType ChangeType `json:"changeType"`
	BeforeHash *string    `json:"beforeHash"`
	AfterHash  *string    `json:"afterHash"`
	Additions  int        `json:"additions"`
	Deletions  int        `json:"deletions"`
	Binary     bool       `json:"binary"`
}

// ManifestEntry identifies one current file, without content.
type ManifestEntry struct {
	Path   string `json:"path"`
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
	Binary bool   `json:"binary"`
}

// Manifest lists every current file of a session.
type Manifest struct {
	Files []ManifestEntry `json:"files"`
}

// Load modes for the initial session state.
const (
	ModeEager = "eager"
	ModeLazy  = "lazy"
)

// SessionState is everything the viewer needs to render a session.
type SessionState struct {
	Session     Session             `json:"session"`
	Snapshot    *StoredSnapshot     `json:"snapshot"`
	Checkpoints []CheckpointSummary `json:"checkpoints"`
	Mode        string              `json:"mode"`
	// Contents maps hash to content for every current text file, in eager
	// mode only.
	Contents map[string]string `json:"contents,omitempty"`
}

// FileContent is one version of a file.
type FileContent struct {
	Content string `json:"content"`
	Binary  bool   `json:"binary"`
	Hash    string `json:"hash"`
}

// Time formats t the way JavaScript's Date.toISOString does, which is what
// the viewer and CLI expect: UTC with millisecond precision.
func Time(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// TimePtr is [Time] for an optional timestamp.
func TimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := Time(*t)
	return &s
}
