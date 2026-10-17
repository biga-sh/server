package wire

import (
	"math"
	"strings"
	"testing"
)

func TestValidSessionID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"k3f9zq2a":              true,
		"my-project_2":          true,
		"A":                     true,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
		"":                      false,
		"-leading":              false,
		"_leading":              false,
		"has space":             false,
		"dot.dot":               false,
		"../etc":                false,
		"ünicode":               false,
	} {
		if got := ValidSessionID(id); got != want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestValidPath(t *testing.T) {
	t.Parallel()
	for p, ok := range map[string]bool{
		"main.go":            true,
		"src/app/page.tsx":   true,
		".github/ci.yml":     true,
		"weird name (1).txt": true,
		"":                   false,
		"/etc/passwd":        false,
		"../x":               false,
		"a/../b":             false,
		"a/./b":              false,
		"a//b":               false,
		"a/":                 false,
		`a\b`:                false,
		"a\x00b":             false,
		"\xff":               false,
	} {
		if err := ValidPath(p); (err == nil) != ok {
			t.Errorf("ValidPath(%q) = %v, want ok=%v", p, err, ok)
		}
	}
}

func tree(children ...*FileNode) *FileSnapshot {
	return &FileSnapshot{
		SessionID: "s", Timestamp: 1,
		Tree: &FileNode{Type: NodeDirectory, Name: "proj", Path: ".", Children: children},
	}
}

func text(p, content string) *FileNode {
	name := p[strings.LastIndexByte(p, '/')+1:]
	return &FileNode{Type: NodeFile, Name: name, Path: p, Size: int64(len(content)), Hash: HashOf(content), Content: content}
}

var limits = TreeLimits{MaxFiles: 10, MaxFileBytes: 1 << 20}

func TestValidateSnapshot(t *testing.T) {
	t.Parallel()
	upper := text("b.txt", "B")
	upper.Hash = strings.ToUpper(upper.Hash)
	bin := &FileNode{Type: NodeFile, Name: "img.png", Path: "img.png", Size: math.MaxInt64, Hash: HashOf("x"), Binary: true, Content: "ignored"}
	snap := tree(
		&FileNode{Type: NodeDirectory, Name: "src", Path: "src", Children: []*FileNode{text("src/a.go", "package a\n")}},
		upper,
		bin,
	)
	files, err := ValidateSnapshot(snap, "s", limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %+v", files)
	}
	if files[1].Hash != strings.ToLower(upper.Hash) || upper.Hash != files[1].Hash {
		t.Error("hashes should be normalized to lowercase in the tree and the result")
	}
	if files[2].Content != "" || bin.Size != math.MaxInt32 {
		t.Errorf("binary placeholder not normalized: %+v, size %d", files[2], bin.Size)
	}
	if snap.Tree.Children[0].Children[0].Content != "" {
		t.Error("content should be stripped from the tree")
	}
}

func TestValidateSnapshotRejects(t *testing.T) {
	t.Parallel()
	forged := text("a", "real")
	forged.Content = "fake"
	deep := text("a", "x")
	for range MaxTreeDepth + 1 {
		deep = &FileNode{Type: NodeDirectory, Name: "d", Path: "d", Children: []*FileNode{deep}}
	}
	many := make([]*FileNode, limits.MaxFiles+1)
	for i := range many {
		many[i] = text(strings.Repeat("f", i+1), "x")
	}
	for name, snap := range map[string]*FileSnapshot{
		"forged hash":      tree(forged),
		"duplicate":        tree(text("a", "1"), text("a", "2")),
		"misplaced path":   tree(&FileNode{Type: NodeFile, Name: "a", Path: "other/a", Hash: HashOf(""), Content: ""}),
		"traversal name":   tree(&FileNode{Type: NodeFile, Name: "..", Path: "..", Hash: HashOf("")}),
		"slash in name":    tree(&FileNode{Type: NodeFile, Name: "a/b", Path: "a/b", Hash: HashOf("")}),
		"null node":        tree(nil),
		"unknown type":     tree(&FileNode{Type: "symlink", Name: "a", Path: "a"}),
		"bad hash":         tree(&FileNode{Type: NodeFile, Name: "a", Path: "a", Hash: "abc"}),
		"negative size":    tree(&FileNode{Type: NodeFile, Name: "a", Path: "a", Size: -1, Hash: HashOf("")}),
		"too many files":   tree(many...),
		"too deep":         tree(deep),
		"file at the root": {SessionID: "s", Timestamp: 1, Tree: text("a", "x")},
	} {
		if _, err := ValidateSnapshot(snap, "s", limits); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ValidateSnapshot(tree(), "other", limits); err == nil {
		t.Error("a snapshot for another session was accepted")
	}
}

func TestValidateCheckpoint(t *testing.T) {
	t.Parallel()
	h := HashOf("x")
	up := &CheckpointUpload{SessionID: "s", Timestamp: 1, Entries: []CheckpointEntry{
		{Path: "a", ChangeType: Added, Hash: &h, Size: 1, Content: "x"},
		{Path: "b", ChangeType: Deleted, Hash: &h, Content: "leftover"},
	}}
	if err := ValidateCheckpoint(up, "s", 10, 1<<20); err != nil {
		t.Fatal(err)
	}
	if up.Entries[1].Hash != nil || up.Entries[1].Content != "" {
		t.Error("a deletion should carry no hash or content")
	}

	bad := HashOf("y")
	for name, e := range map[string]CheckpointEntry{
		"missing hash": {Path: "a", ChangeType: Modified},
		"forged hash":  {Path: "a", ChangeType: Modified, Hash: &bad, Content: "x"},
		"traversal":    {Path: "../a", ChangeType: Deleted},
		"bad old path": {Path: "a", ChangeType: Renamed, OldPath: "/abs", Hash: &h, Content: "x"},
		"unknown type": {Path: "a", ChangeType: "moved", Hash: &h, Content: "x"},
		"too large":    {Path: "a", ChangeType: Added, Hash: new(HashOf(strings.Repeat("x", 11))), Content: strings.Repeat("x", 11)},
	} {
		up := &CheckpointUpload{SessionID: "s", Timestamp: 1, Entries: []CheckpointEntry{e}}
		if err := ValidateCheckpoint(up, "s", 10, 10); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tooMany := &CheckpointUpload{SessionID: "s", Timestamp: 1, Entries: make([]CheckpointEntry, 3)}
	if err := ValidateCheckpoint(tooMany, "s", 2, 10); err == nil {
		t.Error("too many entries accepted")
	}
}

func TestValidateSessionInput(t *testing.T) {
	t.Parallel()
	ok := func(s string) *string { return &s }
	if err := ValidateSessionInput(&CreateSessionInput{Title: ok("my project"), Description: ok("line one\nline two"), GroupID: ok("ws-1")}); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]CreateSessionInput{
		"empty title":      {Title: ok("")},
		"long title":       {Title: ok(strings.Repeat("a", MaxTitleRunes+1))},
		"escape in title":  {Title: ok("\x1b]0;pwned\x07")},
		"NUL in group":     {GroupID: ok("a\x00b")},
		"long description": {Description: ok(strings.Repeat("a", MaxDescRunes+1))},
	} {
		if err := ValidateSessionInput(&in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Runes, not bytes: a title of 120 accented letters is fine.
	if err := ValidateSessionInput(&CreateSessionInput{Title: ok(strings.Repeat("é", MaxTitleRunes))}); err != nil {
		t.Errorf("120-rune title rejected: %v", err)
	}
}
