package repository

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func sharedGraphFixture(t *testing.T) (*Store, *storage.MemoryStore, map[string]GitObject, string) {
	t.Helper()
	store, objects := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("graph-cache", false)); err != nil {
		t.Fatal(err)
	}
	gitObjects := make(map[string]GitObject)
	add := func(object GitObject) string {
		id := GitObjectID(object)
		gitObjects[id] = object
		return id
	}
	blob := add(GitObject{Type: "blob", Data: []byte("shared file\n")})
	tree := add(GitObject{Type: "tree", Data: graphTreeEntry(t, "100644", "file", blob)})
	commit := func(message string, parents ...string) string {
		var text strings.Builder
		text.WriteString("tree " + tree + "\n")
		for _, parent := range parents {
			text.WriteString("parent " + parent + "\n")
		}
		text.WriteString("author Test <test@example.test> 1 +0000\ncommitter Test <test@example.test> 1 +0000\n\n" + message + "\n")
		return add(GitObject{Type: "commit", Data: []byte(text.String())})
	}
	root := commit("root")
	left, right := commit("left", root), commit("right", root)
	merge := commit("merge", left, right)
	tag := add(GitObject{Type: "tag", Data: []byte("object " + merge + "\ntype commit\ntag v1\n\nRelease\n")})
	base, err := store.ReadGitReferences(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishGit(t.Context(), base, []RefUpdate{
		{Name: "refs/heads/main", New: merge}, {Name: "refs/tags/v1", New: tag},
	}, gitObjects, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	base, err = store.ReadGitReferences(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	manifest := base.original.manifest
	manifest.Objects = maps.Clone(manifest.Objects)
	orphan, err := store.putObject(t.Context(), base.original.metadata.ID, "blob", []byte("unpublished secret"), &manifest)
	if err != nil {
		t.Fatal(err)
	}
	publishGraphManifest(t, store, base, manifest)
	return store, objects, gitObjects, orphan
}

func publishGraphManifest(t *testing.T, store *Store, base *GitSnapshot, manifest objectManifest) {
	t.Helper()
	key, err := store.putSnapshot(t.Context(), base.original.metadata.ID, "manifest", manifest)
	if err != nil {
		t.Fatal(err)
	}
	next := base.original.state
	next.Generation++
	next.PackManifest = key
	if err := store.repositories.CompareAndSwapState(t.Context(), base.original.metadata.ID, base.original.version, next); err != nil {
		t.Fatal(err)
	}
}

func TestCachedGraphMergeTagsOrphansAndBudgetFallback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		budget int64
	}{
		{name: "disabled memory", budget: 0},
		{name: "graph larger than budget", budget: 128},
		{name: "retained graph", budget: 1 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, objects, gitObjects, orphan := sharedGraphFixture(t)
			store, err := New(objects, WithCache(repositoryTestCache(t, test.budget, 0)))
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				base, err := store.ReadGitReferences(t.Context(), "alice", "graph-cache")
				if err != nil {
					t.Fatal(err)
				}
				reader, err := store.OpenGit(t.Context(), base)
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Validate(t.Context()); err != nil {
					t.Fatal(err)
				}
				if base.HasObject(orphan) {
					t.Fatal("orphan accepted as common fetch history")
				}
				for _, refs := range []map[string]string{base.References, {"refs/tags/v1": base.References["refs/tags/v1"]}} {
					ids, err := reader.Reachable(t.Context(), refs)
					if err != nil || !slices.Equal(ids, slices.Sorted(maps.Keys(gitObjects))) {
						t.Fatalf("merge/tag reachability = %v, error=%v", ids, err)
					}
				}
				if _, err := reader.Reachable(t.Context(), map[string]string{"refs/tags/orphan": orphan}); !errors.Is(err, ErrInvalid) {
					t.Fatalf("unpublished object returned: %v", err)
				}
				for id, object := range gitObjects {
					if object.Type == "blob" {
						if _, err := reader.Reachable(t.Context(), map[string]string{"refs/heads/wrong-type": id}); !errors.Is(err, ErrInvalid) {
							t.Fatalf("blob accepted as branch commit: %v", err)
						}
					}
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCachedGraphRejectsChangedManifestWithMissingParent(t *testing.T) {
	t.Parallel()
	original, objects, gitObjects, _ := sharedGraphFixture(t)
	store, err := New(objects, WithCache(repositoryTestCache(t, 1<<20, 0)))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenGit(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := base.original.manifest
	manifest.Objects = maps.Clone(manifest.Objects)
	for id, object := range gitObjects {
		if object.Type == "commit" && strings.HasSuffix(string(object.Data), "\nroot\n") {
			delete(manifest.Objects, id)
		}
	}
	publishGraphManifest(t, original, base, manifest)
	next, err := store.ReadGitReferences(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	reader, err = store.OpenGit(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, reader)
	if err := reader.Validate(t.Context()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("previously cached graph concealed missing parent: %v", err)
	}
}

func TestCachedGraphRejectsMutatedPublicRefs(t *testing.T) {
	t.Parallel()
	_, objects, _, _ := sharedGraphFixture(t)
	store, err := New(objects, WithCache(repositoryTestCache(t, 1<<20, 0)))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	delete(base.References, "refs/tags/v1")
	reader, err := store.OpenGit(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, reader)
	if err := reader.Validate(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mutable caller refs poisoned shared graph: %v", err)
	}
}

func TestCachedObjectRechecksChangedVerificationMetadata(t *testing.T) {
	t.Parallel()
	_, objects, _, _ := sharedGraphFixture(t)
	store, err := New(objects, WithCache(repositoryTestCache(t, 1<<20, 1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.load(t.Context(), "alice", "graph-cache")
	if err != nil {
		t.Fatal(err)
	}
	head := snap.refs.Refs["refs/heads/main"]
	if _, err := store.object(t.Context(), snap, head, "commit"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*objectInfo)
	}{
		{name: "size", mutate: func(info *objectInfo) { info.Size++ }},
		{name: "CRC", mutate: func(info *objectInfo) { info.CRC32 ^= 1 }},
		{name: "offset", mutate: func(info *objectInfo) { info.Offset++ }},
		{name: "length", mutate: func(info *objectInfo) { info.Length-- }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := snap
			changed.manifest.Objects = maps.Clone(snap.manifest.Objects)
			info := changed.manifest.Objects[head]
			test.mutate(&info)
			changed.manifest.Objects[head] = info
			if _, err := store.object(t.Context(), changed, head, "commit"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("warm object accepted inconsistent %s: %v", test.name, err)
			}
		})
	}
}
