package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidationError reports a malformed request. It is safe to show to the
// client: it names the offending field and never echoes stored data.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Reason
	}
	return e.Field + ": " + e.Reason
}

func invalid(field, format string, args ...any) *ValidationError {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Limits on request content.
const (
	MaxPathBytes   = 4096
	MaxNameBytes   = 1024
	MaxTreeDepth   = 256
	MaxTitleRunes  = 120
	MaxDescRunes   = 500
	MaxGroupIDLen  = 64
	maxStoredValue = math.MaxInt32
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidSessionID reports whether id is a well-formed session ID: a letter
// or digit, then up to 63 letters, digits, '_' or '-'. The same rule the CLI
// applies to --session.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// ValidHash reports whether h is a hex SHA-256 digest.
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := range len(h) {
		c := h[i]
		isHex := '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
		if !isHex {
			return false
		}
	}
	return true
}

// HashOf is the hex SHA-256 of s, the key a text file's content is stored
// under.
func HashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ValidPath checks a file path from a client: relative, slash-separated,
// with no empty, "." or ".." segments, and nothing Postgres cannot store.
func ValidPath(p string) error {
	switch {
	case p == "":
		return errEmpty
	case len(p) > MaxPathBytes:
		return fmt.Errorf("longer than %d bytes", MaxPathBytes)
	case !utf8.ValidString(p):
		return errUTF8
	case strings.ContainsRune(p, 0):
		return errNUL
	case strings.ContainsRune(p, '\\'):
		return errBackslash
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative")
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			return errors.New("has an empty segment")
		case ".", "..":
			return fmt.Errorf("has a %q segment", seg)
		}
	}
	return nil
}

var (
	errEmpty     = errors.New("is empty")
	errUTF8      = errors.New("is not valid UTF-8")
	errNUL       = errors.New("contains a NUL byte")
	errBackslash = errors.New("must use forward slashes")
)

// File is a validated file from a snapshot or a checkpoint entry.
type File struct {
	Path   string
	Hash   string
	Size   int64
	Binary bool
	// Content is the file's text; empty for binary files.
	Content string
}

// TreeLimits bounds a snapshot tree.
type TreeLimits struct {
	MaxFiles     int
	MaxFileBytes int64
}

// ValidateSnapshot checks an uploaded snapshot against the session it was
// sent to and returns its files. It normalizes the tree in place: hashes are
// lowercased, binary content dropped and sizes clamped to what the database
// stores.
func ValidateSnapshot(s *FileSnapshot, sessionID string, lim TreeLimits) ([]File, error) {
	if s.SessionID != sessionID {
		return nil, invalid("sessionId", "does not match the session in the URL")
	}
	if s.Timestamp <= 0 {
		return nil, invalid("timestamp", "must be a positive number of milliseconds")
	}
	if s.SequenceNum < 0 || s.SequenceNum > maxStoredValue {
		return nil, invalid("sequenceNum", "is out of range")
	}
	if s.Tree == nil {
		return nil, invalid("tree", "is required")
	}
	if s.Tree.Type != NodeDirectory {
		return nil, invalid("tree", "root must be a directory")
	}
	if err := validName(s.Tree.Name); err != nil {
		return nil, invalid("tree.name", "%v", err)
	}
	if s.Tree.Path == "" || len(s.Tree.Path) > MaxPathBytes || strings.ContainsRune(s.Tree.Path, 0) || !utf8.ValidString(s.Tree.Path) {
		return nil, invalid("tree.path", "is malformed")
	}
	w := treeWalker{lim: lim, seen: make(map[string]bool)}
	for _, child := range s.Tree.Children {
		if err := w.walk(child, "", 1); err != nil {
			return nil, err
		}
	}
	return w.files, nil
}

type treeWalker struct {
	lim   TreeLimits
	seen  map[string]bool
	files []File
}

func (w *treeWalker) walk(n *FileNode, parent string, depth int) error {
	if n == nil {
		return invalid("tree", "has a null node")
	}
	if depth > MaxTreeDepth {
		return invalid("tree", "is nested deeper than %d levels", MaxTreeDepth)
	}
	if err := validName(n.Name); err != nil {
		return invalid("tree", "node %q: name %v", n.Path, err)
	}
	want := n.Name
	if parent != "" {
		want = parent + "/" + n.Name
	}
	if n.Path != want {
		return invalid("tree", "node %q: path does not match its place in the tree", n.Path)
	}
	if err := ValidPath(n.Path); err != nil {
		return invalid("tree", "node %q: path %v", n.Path, err)
	}
	if w.seen[n.Path] {
		return invalid("tree", "path %q appears twice", n.Path)
	}
	w.seen[n.Path] = true

	switch n.Type {
	case NodeDirectory:
		n.Content, n.Hash, n.Size, n.Binary = "", "", 0, false
		for _, child := range n.Children {
			if err := w.walk(child, n.Path, depth+1); err != nil {
				return err
			}
		}
		return nil
	case NodeFile:
		n.Children, n.Ignored = nil, false
		f, err := checkFile("tree", n.Path, n.Hash, n.Size, n.Binary, n.Content, w.lim.MaxFileBytes)
		if err != nil {
			return err
		}
		if len(w.files) >= w.lim.MaxFiles {
			return invalid("tree", "has more than %d files", w.lim.MaxFiles)
		}
		n.Hash, n.Size, n.Content = f.Hash, f.Size, ""
		w.files = append(w.files, f)
		return nil
	default:
		return invalid("tree", "node %q: unknown type %q", n.Path, n.Type)
	}
}

func validName(name string) error {
	switch {
	case name == "":
		return errEmpty
	case len(name) > MaxNameBytes:
		return fmt.Errorf("is longer than %d bytes", MaxNameBytes)
	case !utf8.ValidString(name):
		return errUTF8
	case strings.ContainsRune(name, 0):
		return errNUL
	case strings.ContainsRune(name, '/'):
		return errors.New("contains a slash")
	}
	return nil
}

// checkFile validates one file's identity and content. A text file's hash
// must be the SHA-256 of its content: the server serves blobs by hash as
// immutable, so a mismatched pair would poison every cache in front of it.
func checkFile(field, path, hash string, size int64, binary bool, content string, maxBytes int64) (File, error) {
	if !ValidHash(hash) {
		return File{}, invalid(field, "file %q: hash must be 64 hex digits", path)
	}
	hash = strings.ToLower(hash)
	if size < 0 {
		return File{}, invalid(field, "file %q: size is negative", path)
	}
	// Sizes are display metadata stored in a 32-bit column; a larger binary
	// placeholder is recorded at the column's maximum instead of failing.
	size = min(size, maxStoredValue)
	if binary {
		return File{Path: path, Hash: hash, Size: size, Binary: true}, nil
	}
	if int64(len(content)) > maxBytes {
		return File{}, invalid(field, "file %q: content is larger than %d bytes", path, maxBytes)
	}
	if HashOf(content) != hash {
		return File{}, invalid(field, "file %q: hash does not match its content", path)
	}
	return File{Path: path, Hash: hash, Size: size, Content: content}, nil
}

// ValidateCheckpoint checks a checkpoint upload against the session it was
// sent to. It normalizes entries in place, as [ValidateSnapshot] does.
func ValidateCheckpoint(u *CheckpointUpload, sessionID string, maxEntries int, maxFileBytes int64) error {
	if u.SessionID != sessionID {
		return invalid("sessionId", "does not match the session in the URL")
	}
	if u.Timestamp <= 0 {
		return invalid("timestamp", "must be a positive number of milliseconds")
	}
	switch {
	case len(u.Entries) == 0:
		return invalid("entries", "must not be empty")
	case len(u.Entries) > maxEntries:
		return invalid("entries", "has more than %d entries; split the change into several checkpoints", maxEntries)
	}
	seen := make(map[string]bool, len(u.Entries))
	for i := range u.Entries {
		e := &u.Entries[i]
		if err := ValidPath(e.Path); err != nil {
			return invalid("entries", "path %q %v", e.Path, err)
		}
		if seen[e.Path] {
			return invalid("entries", "path %q appears twice", e.Path)
		}
		seen[e.Path] = true
		if e.OldPath != "" {
			if err := ValidPath(e.OldPath); err != nil {
				return invalid("entries", "oldPath %q %v", e.OldPath, err)
			}
		}
		if !e.ChangeType.Valid() {
			return invalid("entries", "file %q: unknown change type %q", e.Path, e.ChangeType)
		}
		if e.ChangeType == Deleted {
			e.Hash, e.Content, e.Size, e.Binary = nil, "", 0, false
			continue
		}
		if e.Hash == nil {
			return invalid("entries", "file %q: hash is required", e.Path)
		}
		f, err := checkFile("entries", e.Path, *e.Hash, e.Size, e.Binary, e.Content, maxFileBytes)
		if err != nil {
			return err
		}
		e.Hash, e.Size, e.Content = &f.Hash, f.Size, f.Content
	}
	return nil
}

// ValidateSessionInput checks a session create or update body.
func ValidateSessionInput(in *CreateSessionInput) error {
	if in.Title != nil {
		if err := validText(*in.Title, 1, MaxTitleRunes, false); err != nil {
			return invalid("title", "%v", err)
		}
	}
	if in.Description != nil {
		if err := validText(*in.Description, 0, MaxDescRunes, true); err != nil {
			return invalid("description", "%v", err)
		}
	}
	if in.GroupID != nil {
		if err := validText(*in.GroupID, 1, MaxGroupIDLen, false); err != nil {
			return invalid("groupId", "%v", err)
		}
	}
	return nil
}

func validText(s string, minRunes, maxRunes int, multiline bool) error {
	n := utf8.RuneCountInString(s)
	switch {
	case !utf8.ValidString(s):
		return errUTF8
	case n < minRunes:
		return fmt.Errorf("must be at least %d characters", minRunes)
	case n > maxRunes:
		return fmt.Errorf("must be at most %d characters", maxRunes)
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return errors.New("contains a control character")
		}
	}
	return nil
}
