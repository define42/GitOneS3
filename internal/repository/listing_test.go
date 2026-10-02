package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestListRepositoriesPageLargeCatalog(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	for i := range 1001 {
		if _, err := store.Create(t.Context(), "alice", createInput(fmt.Sprintf("repo-%04d", i), false)); err != nil {
			t.Fatal(err)
		}
	}
	after := ""
	total := 0
	for {
		page, err := store.ListRepositoriesPage(t.Context(), "alice", after, MaxListPageSize)
		if err != nil || len(page.Repositories) > MaxListPageSize {
			t.Fatalf("page after %q: %+v, %v", after, page, err)
		}
		for _, repo := range page.Repositories {
			if repo.Name != fmt.Sprintf("repo-%04d", total) || !repo.IsEmpty {
				t.Fatalf("repository %d: %+v", total, repo)
			}
			total++
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor != page.Repositories[len(page.Repositories)-1].Name || page.NextCursor == after {
			t.Fatalf("invalid next cursor: %q", page.NextCursor)
		}
		after = page.NextCursor
	}
	if total != 1001 {
		t.Fatalf("listed %d repositories, want 1001", total)
	}
}

func TestListRepositoriesPageCurrentMetadata(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	// State overrides the original metadata default branch. A tag alone also
	// makes a repository nonempty, even when it has no branch refs.
	refsKey, err := store.snapshotKey(t.Context(), metadata.ID, "refs", refsSnapshot{SchemaVersion: 1, Refs: map[string]string{"refs/tags/initial": base.References["refs/heads/main"]}})
	if err != nil {
		t.Fatal(err)
	}
	next := base.original.state
	next.Generation++
	next.DefaultBranch, next.RefsSnapshot = "refs/heads/recovered", refsKey
	if err := store.repositories.CompareAndSwapState(t.Context(), metadata.ID, base.original.version, next); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListRepositoriesPage(t.Context(), "alice", "", 1)
	if err != nil || len(page.Repositories) != 1 || page.Repositories[0].DefaultBranch != "recovered" || page.Repositories[0].IsEmpty {
		t.Fatalf("current metadata: %+v, %v", page, err)
	}
}

func TestListRepositoriesPageBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	for _, test := range []struct {
		name, after string
		limit       int
	}{
		{name: "zero limit", limit: 0},
		{name: "large limit", limit: MaxListPageSize + 1},
		{name: "path cursor", after: "../other", limit: 1},
		{name: "noncanonical cursor", after: "Project", limit: 1},
		{name: "reserved cursor", after: "settings", limit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.ListRepositoriesPage(t.Context(), "alice", test.after, test.limit); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid page accepted: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.ListRepositoriesPage(ctx, "alice", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestListRepositoriesPageStorageKeyOrdering(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	for _, name := range []string{"a", "a-b", "a.b"} {
		if _, err := store.Create(t.Context(), "alice", createInput(name, false)); err != nil {
			t.Fatal(err)
		}
	}
	after := ""
	for i, want := range []string{"a-b", "a.b", "a"} {
		page, err := store.ListRepositoriesPage(t.Context(), "alice", after, 1)
		if err != nil || len(page.Repositories) != 1 || page.Repositories[0].Name != want {
			t.Fatalf("page %d after %q: %+v, %v", i, after, page, err)
		}
		if i < 2 && page.NextCursor != want || i == 2 && page.NextCursor != "" {
			t.Fatalf("page %d next cursor: %q", i, page.NextCursor)
		}
		after = page.NextCursor
	}
}

type catalogPageStore struct {
	storage.ObjectStore
	page storage.ObjectPage
}

func (s *catalogPageStore) ListPage(context.Context, string, string, int) (storage.ObjectPage, error) {
	return s.page, nil
}

func TestListRepositoriesPageRejectsMalformedStoragePages(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		after string
		page  storage.ObjectPage
	}{
		{name: "empty cursor page", page: storage.ObjectPage{NextAfter: metadataKey("alice", "a")}},
		{name: "mismatched cursor", page: storage.ObjectPage{Objects: []storage.ObjectInfo{{Key: metadataKey("alice", "a")}}, NextAfter: metadataKey("alice", "b")}},
		{name: "stale object", after: "a", page: storage.ObjectPage{Objects: []storage.ObjectInfo{{Key: metadataKey("alice", "a")}}}},
		{name: "other namespace", page: storage.ObjectPage{Objects: []storage.ObjectInfo{{Key: metadataKey("bob", "a")}}}},
		{name: "too many objects", page: storage.ObjectPage{Objects: []storage.ObjectInfo{{Key: metadataKey("alice", "a")}, {Key: metadataKey("alice", "b")}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(&catalogPageStore{ObjectStore: storage.NewMemoryStore(), page: test.page})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ListRepositoriesPage(t.Context(), "alice", test.after, 1); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("malformed page accepted: %v", err)
			}
		})
	}
}

type catalogReadStore struct {
	storage.ObjectStore
	delay   time.Duration
	reads   atomic.Int64
	active  atomic.Int64
	maximum atomic.Int64
}

func (s *catalogReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "-manifest-") || strings.Contains(key, "/packs/") || strings.Contains(key, "/objects/") {
		return nil, storage.ObjectInfo{}, errors.New("catalog fetched Git object data")
	}
	s.reads.Add(1)
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for prior := s.maximum.Load(); active > prior; prior = s.maximum.Load() {
		if s.maximum.CompareAndSwap(prior, active) {
			break
		}
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, storage.ObjectInfo{}, ctx.Err()
	case <-timer.C:
		return s.ObjectStore.Get(ctx, key)
	}
}

func TestListRepositoriesPageBoundedStorageReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, objects := newTestStore(t)
		for i := range MaxListPageSize {
			if _, err := store.Create(t.Context(), "alice", createInput(fmt.Sprintf("repo-%02d", i), false)); err != nil {
				t.Fatal(err)
			}
		}
		observed := &catalogReadStore{ObjectStore: objects, delay: 50 * time.Millisecond}
		store, err := New(observed)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		page, err := store.ListRepositoriesPage(ctx, "alice", "", MaxListPageSize)
		if err != nil || len(page.Repositories) != MaxListPageSize {
			t.Fatalf("page exceeded deadline: %d, %v", len(page.Repositories), err)
		}
		if observed.reads.Load() != 3*MaxListPageSize || observed.maximum.Load() > listMetadataWorkers {
			t.Fatalf("unbounded reads: total %d, concurrent %d", observed.reads.Load(), observed.maximum.Load())
		}
		if observed.active.Load() != 0 {
			t.Fatal("catalog returned before readers exited")
		}
		interrupted, stop := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer stop()
		if _, err := store.ListRepositoriesPage(interrupted, "alice", "", MaxListPageSize); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("in-flight deadline lost: %v", err)
		}
		if observed.active.Load() != 0 {
			t.Fatal("canceled catalog left readers running")
		}
	})
}
