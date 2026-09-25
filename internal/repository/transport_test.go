package repository

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func transportFixture(t *testing.T) (*Store, *GitSnapshot) {
	t.Helper()
	store, _ := newTestStore(t)
	if _, err := store.Create(context.Background(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGit(context.Background(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return store, base
}

func TestGitObjectID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		object GitObject
		want   string
	}{
		{name: "empty blob", object: GitObject{Type: "blob"}, want: "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{name: "blob reference vector", object: GitObject{Type: "blob", Data: []byte("test content\n")}, want: "d670460b4b4aece5915caf5c68d12f560a9fe3e4"},
		{name: "empty tree", object: GitObject{Type: "tree"}, want: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := GitObjectID(tt.object); got != tt.want {
				t.Fatalf("GitObjectID() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPublishGitAtomicAndCAS(t *testing.T) {
	t.Parallel()
	store, base := transportFixture(t)
	ctx := context.Background()
	head := base.References["refs/heads/main"]
	stale, err := store.ReadGit(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	updates := []RefUpdate{{Name: "refs/heads/feature", New: head}, {Name: "refs/tags/v1", New: head}}
	authorized := 0
	if err := store.PublishGit(ctx, base, updates, nil, func(context.Context) error { authorized++; return nil }); err != nil {
		t.Fatal(err)
	}
	if authorized != 1 {
		t.Fatalf("rechecks = %d", authorized)
	}
	current, err := store.ReadGit(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if current.References["refs/heads/feature"] != head || current.References["refs/tags/v1"] != head {
		t.Fatal("multi-ref publication incomplete")
	}
	if err := store.PublishGit(ctx, stale, []RefUpdate{{Name: "refs/heads/other", New: head}}, nil, func(context.Context) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale publication = %v", err)
	}
	current, err = store.ReadGit(ctx, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := current.References["refs/heads/other"]; ok {
		t.Fatal("stale reference became visible")
	}
	if err := store.PublishGit(ctx, current, []RefUpdate{{Name: "refs/heads/feature", Old: head}, {Name: "refs/tags/v1", Old: head}}, nil, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	branches, err := store.Branches(ctx, "alice", "demo")
	if err != nil || len(branches) != 1 {
		t.Fatalf("branches = %v %v", branches, err)
	}
}

func TestPublishGitRejectsUpdates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing authorization", "revoked", "stale old value", "missing object", "duplicate ref", "invalid ref", "ref directory conflict", "invalid commit"} {
		t.Run(name, func(t *testing.T) {
			store, base := transportFixture(t)
			head := base.References["refs/heads/main"]
			updates := []RefUpdate{{Name: "refs/heads/feature", New: head}}
			incoming := map[string]GitObject{}
			authorize := func(context.Context) error { return nil }
			switch name {
			case "missing authorization":
				authorize = nil
			case "revoked":
				authorize = func(context.Context) error { return errors.New("revoked") }
			case "stale old value":
				updates[0].Old = head
			case "missing object":
				updates[0].New = strings.Repeat("a", 40)
			case "duplicate ref":
				updates = append(updates, updates[0])
			case "invalid ref":
				updates[0].Name = "refs/heads/../bad"
			case "ref directory conflict":
				updates[0].Name = "refs/heads/main/sub"
			case "invalid commit":
				object := GitObject{Type: "commit", Data: []byte("tree " + strings.Repeat("a", 40) + "\n\ninvalid\n")}
				id := GitObjectID(object)
				incoming[id] = object
				updates[0].New = id
			}
			if err := store.PublishGit(context.Background(), base, updates, incoming, authorize); err == nil {
				t.Fatal("invalid push accepted")
			}
			current, err := store.ReadGit(context.Background(), "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			if len(current.References) != 1 || current.References["refs/heads/main"] != head {
				t.Fatal("failed push altered refs")
			}
		})
	}
}

func TestPublishGitFailedDurabilityAndFinalAuthorization(t *testing.T) {
	t.Parallel()
	store, base := transportFixture(t)
	head := base.References["refs/heads/main"]
	failed, err := New(failingStore{ObjectStore: store.objects, fail: "/state"})
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.PublishGit(context.Background(), base, []RefUpdate{{Name: "refs/heads/feature", New: head}}, nil, func(context.Context) error { return nil }); err == nil {
		t.Fatal("failed CAS acknowledged")
	}
	current, err := store.ReadGit(context.Background(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.References) != 1 {
		t.Fatal("failed state write published refs")
	}
	// Once the recheck runs, every immutable target of the next state is present,
	// but the authoritative generation is still unchanged.
	checked := false
	err = store.PublishGit(context.Background(), base, []RefUpdate{{Name: "refs/heads/feature", New: head}}, nil, func(ctx context.Context) error {
		checked = true
		state, _, err := store.repositories.LoadState(ctx, base.original.metadata.ID)
		if err != nil {
			return err
		}
		if state.Generation != base.original.state.Generation {
			t.Error("publication preceded authorization")
		}
		return errors.New("token revoked during upload")
	})
	if !checked || !errors.Is(err, ErrForbidden) {
		t.Fatalf("final authorization = %v %v", checked, err)
	}
	current, err = store.ReadGit(context.Background(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.References) != 1 {
		t.Fatal("revoked push published refs")
	}
}

func TestPublishGitPreservesForbiddenClassification(t *testing.T) {
	t.Parallel()
	store, base := transportFixture(t)
	head := base.References["refs/heads/main"]
	err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/feature", New: head}}, nil, func(context.Context) error {
		return ErrInvalid
	})
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvalid) {
		t.Fatalf("authorization error classification = %v", err)
	}
}

func TestReadGitCorruption(t *testing.T) {
	t.Parallel()
	store, base := transportFixture(t)
	if err := store.objects.Delete(context.Background(), objectKey(base.original.metadata.ID, base.References["refs/heads/main"]), storage.Version("")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadGit(context.Background(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing object = %v", err)
	}
}

func TestReadGitInvalidGraphIsCorruption(t *testing.T) {
	t.Parallel()
	store, base := transportFixture(t)
	manifest := base.original.manifest
	for id, info := range manifest.Objects {
		if info.Type == "tree" {
			delete(manifest.Objects, id)
		}
	}
	manifestKey, err := store.putSnapshot(t.Context(), base.original.metadata.ID, "manifest", manifest)
	if err != nil {
		t.Fatal(err)
	}
	next := base.original.state
	next.Generation++
	next.PackManifest = manifestKey
	if err := store.repositories.CompareAndSwapState(t.Context(), base.original.metadata.ID, base.original.version, next); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadGit(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrInvalid) {
		t.Fatalf("persisted graph error classification = %v", err)
	}
}
