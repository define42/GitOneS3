package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func newTestStore(t *testing.T) (*Store, *storage.MemoryStore) {
	t.Helper()
	objects := storage.NewMemoryStore()
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	return store, objects
}

func createInput(name string, readme bool) CreateInput {
	return CreateInput{Name: name, Description: "A private repository", InitializeReadme: readme, CreatedBy: "user:alice", AuthorName: "Alice", AuthorEmail: "alice@users.gitone.invalid"}
}

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatal("nil object store accepted")
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		valid bool
	}{
		{"a", true}, {"my-repo", true}, {"my_repo.v2", true}, {strings.Repeat("a", 63), true},
		{"", false}, {"Main", false}, {"repo.git", false}, {"repo..name", false}, {"../repo", false},
		{"-repo", false}, {"repo-", false}, {".repo", false}, {"repo.", false}, {"repo/other", false},
		{"repo%2fother", false}, {"auth", false}, {"settings", false}, {"members", false}, {"invitations", false},
		{strings.Repeat("a", 64), false}, {"répo", false},
	} {
		t.Run(fmt.Sprintf("%q", tt.name), func(t *testing.T) {
			if got := ValidName(tt.name); got != tt.valid {
				t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.valid)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()
	for _, readme := range []bool{false, true} {
		t.Run(fmt.Sprintf("readme %v", readme), func(t *testing.T) {
			store, objects := newTestStore(t)
			ctx := context.Background()
			created, err := store.Create(ctx, "alice", createInput("demo", readme))
			if err != nil {
				t.Fatal(err)
			}
			if created.Namespace != "alice" || created.Name != "demo" || created.DefaultBranch != "main" || created.IsEmpty == readme || created.Visibility != "private" || created.CreatedAt.IsZero() || !idPattern.MatchString(created.ID) {
				t.Fatalf("unexpected metadata: %+v", created)
			}
			// A fresh store proves that browsing does not rely on process-local state.
			restarted, err := New(objects)
			if err != nil {
				t.Fatal(err)
			}
			got, err := restarted.Get(ctx, "alice", "demo")
			if err != nil || got != created {
				t.Fatalf("Get = %+v, %v; want %+v", got, err, created)
			}
			branches, err := restarted.Branches(ctx, "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			tree, err := restarted.Tree(ctx, "alice", "demo", "", "")
			if err != nil {
				t.Fatal(err)
			}
			commits, err := restarted.Commits(ctx, "alice", "demo", "")
			if err != nil {
				t.Fatal(err)
			}
			if !readme {
				if branches == nil || len(branches) != 0 || tree.Entries == nil || len(tree.Entries) != 0 || tree.Commit != "" || commits == nil || len(commits) != 0 {
					t.Fatalf("invalid empty repository: %+v %+v %+v", branches, tree, commits)
				}
				if _, err := restarted.Blob(ctx, "alice", "demo", "", "README.md"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("empty blob error = %v", err)
				}
				return
			}
			if len(branches) != 1 || branches[0].Name != "main" || !objectIDPattern.MatchString(branches[0].Commit) || len(tree.Entries) != 1 || tree.Entries[0].Name != "README.md" || tree.Entries[0].Type != "file" {
				t.Fatalf("unexpected branch/tree: %+v %+v", branches, tree)
			}
			blob, err := restarted.Blob(ctx, "alice", "demo", "main", "README.md")
			if err != nil {
				t.Fatal(err)
			}
			if blob.Content != "# demo\n\nA private repository\n" || blob.IsBinary || blob.Size != int64(len(blob.Content)) || blob.Commit != branches[0].Commit {
				t.Fatalf("unexpected blob: %+v", blob)
			}
			if len(commits) != 1 || commits[0].Message != "Initial commit" || commits[0].AuthorName != "Alice" || !commits[0].CreatedAt.Equal(created.CreatedAt) || len(commits[0].Parents) != 0 || commits[0].ID != blob.Commit {
				t.Fatalf("unexpected commits: %+v", commits)
			}
		})
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, namespace string
		mutate          func(*CreateInput)
	}{
		{"invalid namespace", "../alice", func(*CreateInput) {}},
		{"reserved namespace", "api", func(*CreateInput) {}},
		{"auth namespace", "auth", func(*CreateInput) {}},
		{"invalid name", "alice", func(i *CreateInput) { i.Name = "Demo" }},
		{"invalid branch", "alice", func(i *CreateInput) { i.DefaultBranch = "../main" }},
		{"long description", "alice", func(i *CreateInput) { i.Description = strings.Repeat("ø", 501) }},
		{"control description", "alice", func(i *CreateInput) { i.Description = "first\x00second" }},
		{"missing creator", "alice", func(i *CreateInput) { i.CreatedBy = "" }},
		{"author newline", "alice", func(i *CreateInput) { i.AuthorName = "Alice\nparent evil" }},
		{"author email delimiter", "alice", func(i *CreateInput) { i.AuthorEmail = "alice@example.com>" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, objects := newTestStore(t)
			input := createInput("demo", true)
			tt.mutate(&input)
			if _, err := store.Create(context.Background(), tt.namespace, input); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
			stored, err := objects.List(context.Background(), "r")
			if err != nil || len(stored) != 0 {
				t.Fatalf("invalid request wrote objects: %v %v", stored, err)
			}
		})
	}
	store, _ := newTestStore(t)
	input := createInput("unicode", true)
	input.Description = strings.Repeat("ø", 500)
	input.DefaultBranch = "features/start"
	if _, err := store.Create(context.Background(), "alice", input); err != nil {
		t.Fatalf("valid unicode description and branch: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	ctx := context.Background()
	first, err := store.Create(ctx, "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	before, err := objects.List(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "alice", createInput("demo", false)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	after, err := objects.List(ctx, "r")
	if err != nil || len(after) != len(before) {
		t.Fatalf("duplicate generated new objects: %v", err)
	}
	got, err := store.Get(ctx, "alice", "demo")
	if err != nil || got != first {
		t.Fatalf("duplicate changed metadata: %+v %v", got, err)
	}
	if _, err := store.Create(ctx, "bob", createInput("demo", false)); err != nil {
		t.Fatalf("same name in another namespace: %v", err)
	}
}

func TestCreateRace(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	const writers = 12
	results := make(chan error, writers)
	var workers sync.WaitGroup
	for range writers {
		workers.Go(func() {
			_, err := store.Create(ctx, "alice", createInput("race", true))
			results <- err
		})
	}
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrAlreadyExists) {
			t.Errorf("unexpected race error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	if _, err := store.Blob(ctx, "alice", "race", "", "README.md"); err != nil {
		t.Fatalf("winner incomplete: %v", err)
	}
}

type failingStore struct {
	storage.ObjectStore
	fail string
}

func (s failingStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.ObjectInfo, error) {
	isFailure := strings.Contains(key, s.fail)
	if s.fail == "/state" {
		isFailure = strings.HasSuffix(key, s.fail)
	}
	if isFailure {
		return storage.ObjectInfo{}, errors.New("injected storage failure")
	}
	return s.ObjectStore.Put(ctx, key, body, size, options)
}

func TestCreateFailureDoesNotClaimName(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"/objects/", "-refs-", "-manifest-", "/state", "repositories/"} {
		t.Run(failure, func(t *testing.T) {
			objects := storage.NewMemoryStore()
			store, err := New(failingStore{ObjectStore: objects, fail: failure})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(context.Background(), "alice", createInput("retry", true)); err == nil {
				t.Fatal("injected failure ignored")
			}
			if _, err := objects.Head(context.Background(), metadataKey("alice", "retry")); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("failed creation claimed name: %v", err)
			}
			retry, err := New(objects)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := retry.Create(context.Background(), "alice", createInput("retry", true)); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestListAndGet(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	empty, err := store.List(ctx, "alice")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, %v", empty, err)
	}
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := store.Create(ctx, "alice", createInput(name, false)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Create(ctx, "bob", createInput("hidden", true)); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx, "alice")
	if err != nil || len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := store.Get(ctx, "alice", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.List(ctx, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestReadRejectsCorruptMetadata(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{")},
		{"unknown field", []byte(`{"schemaVersion":1,"extra":true}`)},
		{"oversized", bytes.Repeat([]byte("x"), maxJSONBytes+1)},
		{"trailing json", []byte(`{} {}`)},
		{"invalid identity", []byte(`{"schemaVersion":1,"repository":{"id":"../../bob","namespace":"alice","name":"demo"}}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, objects := newTestStore(t)
			if _, err := objects.Put(context.Background(), metadataKey("alice", "demo"), bytes.NewReader(tt.data), int64(len(tt.data)), storage.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(context.Background(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt metadata error = %v", err)
			}
		})
	}
}
