package repository

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestGitObjectCompatibility(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	metadata, err := store.Create(ctx, "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.load(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	manifest := objectManifest{Objects: map[string]objectInfo{}}
	id, err := store.putObject(ctx, metadata.ID, "blob", []byte("test content\n"), &manifest)
	if err != nil {
		t.Fatal(err)
	}
	// The published Git documentation's reference vector verifies framing/SHA-1.
	if id != "d670460b4b4aece5915caf5c68d12f560a9fe3e4" {
		t.Fatalf("git blob id = %s", id)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Log("native git unavailable; reference vector verified")
		return
	}
	for id, info := range snap.manifest.Objects {
		content, err := store.object(ctx, snap, id, info.Type)
		if err != nil {
			t.Fatal(err)
		}
		// git hash-object parses trees and commits before accepting them, without
		// a working tree or object writes. No shell interprets these arguments.
		command := exec.CommandContext(ctx, git, "hash-object", "-t", info.Type, "--stdin")
		command.Stdin = bytes.NewReader(content)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git rejected %s: %s: %v", info.Type, output, err)
		}
		if strings.TrimSpace(string(output)) != id {
			t.Fatalf("native git hash = %s, want %s", output, id)
		}
	}
}

func TestBrowseRejectsPathsAndRefs(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"demo", "empty"} {
		if _, err := store.Create(ctx, "alice", createInput(name, name == "demo")); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"../README.md", "/README.md", "README.md/", "a//b", "a/../README.md", ".git/config", "a/.GIT/config", "a\\b", "a\x00b", strings.Repeat("a", 256), strings.Repeat("a/", 33) + "b"} {
		t.Run(fmt.Sprintf("path %q", path), func(t *testing.T) {
			if _, err := store.Tree(ctx, "alice", "demo", "", path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("tree error = %v", err)
			}
			if _, err := store.Blob(ctx, "alice", "demo", "", path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("blob error = %v", err)
			}
		})
	}
	for _, ref := range []string{"../main", "main^", "refs//heads/main", "main.lock", ".main", "a@{b", "main\n"} {
		t.Run(fmt.Sprintf("ref %q", ref), func(t *testing.T) {
			if _, err := store.Commits(ctx, "alice", "demo", ref); !errors.Is(err, ErrInvalid) {
				t.Fatalf("ref error = %v", err)
			}
		})
	}
	for _, name := range []string{"demo", "empty"} {
		if _, err := store.Tree(ctx, "alice", name, "missing", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing branch = %v", err)
		}
		if _, err := store.Tree(ctx, "alice", name, "", "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing directory = %v", err)
		}
		if _, err := store.Blob(ctx, "alice", name, "", "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing file = %v", err)
		}
	}
	if _, err := store.Tree(ctx, "alice", "demo", "", "README.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tree of blob = %v", err)
	}
}

func TestBrowseNestedAndBinary(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	metadata, err := store.Create(ctx, "alice", createInput("nested", true))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.load(ctx, "alice", "nested")
	if err != nil {
		t.Fatal(err)
	}
	parent := snap.refs.Refs["refs/heads/main"]
	put := func(kind string, data []byte) string {
		t.Helper()
		id, err := store.putObject(ctx, metadata.ID, kind, data, &snap.manifest)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	text := put("blob", []byte("hello world\n"))
	binary := put("blob", []byte{'a', 0, 'b'})
	subtree := put("tree", treeBytes(t, "100644", "hello.txt", text))
	root := put("tree", append(treeBytes(t, "100644", "binary.dat", binary), treeBytes(t, "40000", "docs", subtree)...))
	commit := put("commit", []byte("tree "+root+"\nparent "+parent+"\nauthor Alice <alice@users.gitone.invalid> 1700000000 +0000\ncommitter Alice <alice@users.gitone.invalid> 1700000000 +0000\n\nAdd files\n"))
	snap.refs.Refs["refs/heads/main"] = commit
	refsKey, err := store.putSnapshot(ctx, metadata.ID, "refs", snap.refs)
	if err != nil {
		t.Fatal(err)
	}
	manifestKey, err := store.putSnapshot(ctx, metadata.ID, "manifest", snap.manifest)
	if err != nil {
		t.Fatal(err)
	}
	state, version, err := store.repositories.LoadState(ctx, metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation++
	state.RefsSnapshot, state.PackManifest = refsKey, manifestKey
	if err := store.repositories.CompareAndSwapState(ctx, metadata.ID, version, state); err != nil {
		t.Fatal(err)
	}
	tree, err := store.Tree(ctx, "alice", "nested", "", "")
	if err != nil || len(tree.Entries) != 2 || tree.Entries[0].Name != "docs" || tree.Entries[0].Type != "directory" {
		t.Fatalf("root tree = %+v, %v", tree, err)
	}
	tree, err = store.Tree(ctx, "alice", "nested", "", "docs")
	if err != nil || len(tree.Entries) != 1 || tree.Entries[0].Path != "docs/hello.txt" || tree.Entries[0].Size != 12 {
		t.Fatalf("nested tree = %+v, %v", tree, err)
	}
	blob, err := store.Blob(ctx, "alice", "nested", "", "docs/hello.txt")
	if err != nil || blob.Content != "hello world\n" {
		t.Fatalf("nested file = %+v, %v", blob, err)
	}
	blob, err = store.Blob(ctx, "alice", "nested", "", "binary.dat")
	if err != nil || !blob.IsBinary || blob.Content != "" || blob.Size != 3 {
		t.Fatalf("binary file = %+v, %v", blob, err)
	}
	commits, err := store.Commits(ctx, "alice", "nested", "")
	if err != nil || len(commits) != 2 || commits[0].Message != "Add files" || commits[0].Parents[0] != parent || commits[1].ID != parent {
		t.Fatalf("history = %+v, %v", commits, err)
	}
	if _, err := store.Blob(ctx, "alice", "nested", "", "docs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blob of tree = %v", err)
	}
}

func treeBytes(t *testing.T, mode, name, id string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(mode+" "+name+"\x00"), raw...)
}

func TestBrowseRejectsCorruption(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"blob", "tree", "commit", "refs", "manifest", "state"} {
		t.Run(target, func(t *testing.T) {
			store, objects := newTestStore(t)
			ctx := context.Background()
			metadata, err := store.Create(ctx, "alice", createInput("demo", true))
			if err != nil {
				t.Fatal(err)
			}
			snap, err := store.load(ctx, "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			state, _, err := store.repositories.LoadState(ctx, metadata.ID)
			if err != nil {
				t.Fatal(err)
			}
			key := ""
			switch target {
			case "refs":
				key = "repos/" + metadata.ID + "/" + state.RefsSnapshot
			case "manifest":
				key = "repos/" + metadata.ID + "/" + state.PackManifest
			case "state":
				key = "repos/" + metadata.ID + "/state"
			default:
				for id, info := range snap.manifest.Objects {
					if info.Type == target {
						key = objectKey(metadata.ID, id)
						break
					}
				}
			}
			if key == "" {
				t.Fatal("missing test object")
			}
			if _, err := objects.Put(ctx, key, strings.NewReader("corrupt"), 7, storage.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Blob(ctx, "alice", "demo", "", "README.md"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt %s error = %v", target, err)
			}
		})
	}
}

func TestObjectRejectsDecompressionBomb(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	ctx := context.Background()
	metadata, err := store.Create(ctx, "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.load(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for candidate, info := range snap.manifest.Objects {
		if info.Type == "blob" {
			id = candidate
		}
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("a"), maxObjectBytes*2)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(ctx, objectKey(metadata.ID, id), bytes.NewReader(compressed.Bytes()), int64(compressed.Len()), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Blob(ctx, "alice", "demo", "", "README.md"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("decompression bomb error = %v", err)
	}
}
