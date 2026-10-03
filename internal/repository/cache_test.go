package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/storage"
)

type cacheReadStore struct {
	storage.ObjectStore
	mu   sync.Mutex
	keys []string
}

func (s *cacheReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	s.keys = append(s.keys, key)
	s.mu.Unlock()
	return s.ObjectStore.Get(ctx, key)
}

func (s *cacheReadStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	s.keys = append(s.keys, key)
	s.mu.Unlock()
	return s.ObjectStore.GetRange(ctx, key, offset, length)
}

func (s *cacheReadStore) reset() {
	s.mu.Lock()
	s.keys = nil
	s.mu.Unlock()
}

func (s *cacheReadStore) reads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func repositoryTestCache(t testing.TB, memory, disk int64) *cache.Cache {
	t.Helper()
	shared, err := cache.New(cache.Options{MemoryBytes: memory, DiskBytes: disk, Directory: t.TempDir(), Namespace: t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shared.Close(); err != nil {
			t.Error(err)
		}
	})
	return shared
}

func TestSharedCacheFreshStateAndIntegrityBypass(t *testing.T) {
	t.Parallel()
	original, objects := newTestStore(t)
	if _, err := original.Create(t.Context(), "alice", createInput("cached", true)); err != nil {
		t.Fatal(err)
	}
	observed := &cacheReadStore{ObjectStore: objects}
	store, err := New(observed, WithCache(repositoryTestCache(t, 16<<20, 128<<20)))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Browse(t.Context(), "alice", "cached", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	observed.reset()
	second, err := store.Browse(t.Context(), "alice", "cached", "", "", false)
	if err != nil || second.Readme.Content != first.Readme.Content {
		t.Fatalf("warm browse: %+v %v", second, err)
	}
	assertAuthorityOnlyReads(t, observed.reads())
	// A writer publishes new immutable keys. The next request must immediately
	// observe them even while every earlier generation remains cached.
	publishCachedReadme(t, original, "cached", "updated\n")
	updated, err := store.Browse(t.Context(), "alice", "cached", "", "", false)
	if err != nil || updated.Readme.Content != "updated\n" {
		t.Fatalf("stale generation: %+v %v", updated, err)
	}
	snap, err := original.load(t.Context(), "alice", "cached")
	if err != nil {
		t.Fatal(err)
	}
	key := "repos/" + snap.metadata.ID + "/" + snap.state.PackManifest
	if _, err := objects.Put(t.Context(), key, strings.NewReader("{}"), 2, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Serving uses verified cached immutable bytes; integrity must inspect S3.
	if _, err := store.Browse(t.Context(), "alice", "cached", "", "", false); err != nil {
		t.Fatalf("warm immutable content: %v", err)
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "cached"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("integrity concealed backend corruption: %v", err)
	}
}

func publishCachedReadme(t *testing.T, store *Store, name, content string) {
	t.Helper()
	base, err := store.ReadGitReferences(t.Context(), "alice", name)
	if err != nil {
		t.Fatal(err)
	}
	blob := GitObject{Type: "blob", Data: []byte(content)}
	blobID := GitObjectID(blob)
	tree := GitObject{Type: "tree", Data: graphTreeEntry(t, "100644", "README.md", blobID)}
	treeID := GitObjectID(tree)
	commit := GitObject{Type: "commit", Data: []byte("tree " + treeID + "\nparent " + base.References["refs/heads/main"] + "\nauthor Test <test@example.test> 2 +0000\ncommitter Test <test@example.test> 2 +0000\n\nUpdate\n")}
	head := GitObjectID(commit)
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", Old: base.References["refs/heads/main"], New: head}}, map[string]GitObject{blobID: blob, treeID: tree, head: commit}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func assertAuthorityOnlyReads(t *testing.T, keys []string) {
	t.Helper()
	if len(keys) != 2 {
		t.Fatalf("warm reads=%v; want repository identity and state only", keys)
	}
	for _, key := range keys {
		if strings.Contains(key, "/states/") || strings.Contains(key, "/objects/") || strings.Contains(key, "/packs/") {
			t.Fatalf("warm immutable read: %s", key)
		}
	}
}

func TestSharedGraphAndPackReuse(t *testing.T) {
	t.Parallel()
	original, objects, _, gitObjects := newPackedReadFixture(t)
	observed := &cacheReadStore{ObjectStore: objects}
	store, err := New(observed, WithCache(repositoryTestCache(t, 32<<20, 128<<20)))
	if err != nil {
		t.Fatal(err)
	}
	allIDs := slices.Sorted(maps.Keys(gitObjects))
	for round := range 2 {
		observed.reset()
		base, err := store.ReadGitReferences(t.Context(), "alice", "sparse")
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
		ids, err := reader.Reachable(t.Context(), base.References)
		if err != nil || !slices.Equal(ids, allIDs) {
			t.Fatalf("reachable=%v err=%v", ids, err)
		}
		ids[0] = "caller mutation"
		again, err := reader.Reachable(t.Context(), base.References)
		if err != nil || !slices.Equal(again, allIDs) {
			t.Fatalf("shared reachability mutated: %v %v", again, err)
		}
		if err := reader.Prefetch(t.Context(), allIDs); err != nil {
			t.Fatal(err)
		}
		for _, id := range allIDs {
			object, err := reader.Get(t.Context(), id)
			if err != nil || !bytes.Equal(object.Data, gitObjects[id].Data) {
				t.Fatalf("object %s: %v", id, err)
			}
			if len(object.Data) > 0 {
				object.Data[0] ^= 1
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if round == 1 {
			assertAuthorityOnlyReads(t, observed.reads())
		}
	}
	// Force-push to an earlier commit: reachable object sets must follow the new
	// refs snapshot even when old object bodies and graph indexes remain cached.
	base, err := original.ReadGitReferences(t.Context(), "alice", "sparse")
	if err != nil {
		t.Fatal(err)
	}
	head := base.References["refs/heads/main"]
	var older string
	for id, object := range gitObjects {
		if object.Type == "commit" && id != head {
			older = id
		}
	}
	if err := original.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", Old: head, New: older}}, nil, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	next, err := store.ReadGitReferences(t.Context(), "alice", "sparse")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenGit(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, reader)
	if err := reader.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if next.HasObject(head) {
		t.Fatal("removed history accepted as client common history")
	}
}

func TestSharedCacheDiskRestartAndZeroMemory(t *testing.T) {
	t.Parallel()
	_, objects, _, gitObjects := newPackedReadFixture(t)
	observed := &cacheReadStore{ObjectStore: objects}
	options := cache.Options{DiskBytes: 128 << 20, Directory: t.TempDir(), Namespace: t.Name()}
	for round := range 2 {
		shared, err := cache.New(options)
		if err != nil {
			t.Fatal(err)
		}
		store, err := New(observed, WithCache(shared))
		if err != nil {
			t.Fatal(err)
		}
		observed.reset()
		base, err := store.ReadGitReferences(t.Context(), "alice", "sparse")
		if err != nil {
			t.Fatal(err)
		}
		reader, err := store.OpenGit(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Prefetch(t.Context(), slices.Sorted(maps.Keys(gitObjects))); err != nil {
			t.Fatal(err)
		}
		if err := reader.Validate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Reachable(t.Context(), base.References); err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if err := shared.Close(); err != nil {
			t.Fatal(err)
		}
		if round == 1 {
			assertAuthorityOnlyReads(t, observed.reads())
		}
	}
}
