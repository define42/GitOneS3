package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestRepositoryStore_CompareAndSwapState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	objects := NewMemoryStore()
	store, err := NewRepositoryStore(objects)
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	putStateArtifacts(t, ctx, store, "repo-1", 1)

	initial := testState(1)
	if err := store.CompareAndSwapState(ctx, "repo-1", "", initial); err != nil {
		t.Fatalf("initial CompareAndSwapState() error = %v", err)
	}
	got, version, err := store.LoadState(ctx, "repo-1")
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got != initial {
		t.Fatalf("LoadState() = %#v, want %#v", got, initial)
	}
	if version == "" {
		t.Fatal("LoadState() version is empty")
	}

	putStateArtifacts(t, ctx, store, "repo-1", 2)
	if err := store.CompareAndSwapState(ctx, "repo-1", version, testState(2)); err != nil {
		t.Fatalf("next CompareAndSwapState() error = %v", err)
	}
	if err := store.CompareAndSwapState(ctx, "repo-1", version, testState(2)); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale CompareAndSwapState() error = %v, want ErrPreconditionFailed", err)
	}
}

func TestRepositoryStore_ConcurrentCompareAndSwap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	objects := NewMemoryStore()
	store, err := NewRepositoryStore(objects)
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	putStateArtifacts(t, ctx, store, "repo-1", 1)
	if err := store.CompareAndSwapState(ctx, "repo-1", "", testState(1)); err != nil {
		t.Fatalf("initial CompareAndSwapState() error = %v", err)
	}
	_, version, err := store.LoadState(ctx, "repo-1")
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	putStateArtifacts(t, ctx, store, "repo-1", 2)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			results <- store.CompareAndSwapState(ctx, "repo-1", version, testState(2))
		})
	}
	close(start)
	wait.Wait()
	close(results)

	var successes, conflicts int
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, ErrPreconditionFailed):
			conflicts++
		default:
			t.Fatalf("CompareAndSwapState() unexpected error = %v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS outcomes = %d successes, %d conflicts; want 1 and 1", successes, conflicts)
	}
}

func TestRepositoryStore_RequiresDurableReferences(t *testing.T) {
	t.Parallel()

	store, err := NewRepositoryStore(NewMemoryStore())
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	err = store.CompareAndSwapState(context.Background(), "repo-1", "", testState(1))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CompareAndSwapState() error = %v, want ErrNotFound", err)
	}
}

func TestRepositoryStore_LoadStateRejectsMismatchedSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot string
	}{
		{name: "missing", snapshot: ""},
		{name: "wrong", snapshot: "states/wrong-state.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			objects := NewMemoryStore()
			store, err := NewRepositoryStore(objects)
			if err != nil {
				t.Fatalf("NewRepositoryStore() error = %v", err)
			}
			envelopeJSON, err := json.Marshal(stateEnvelope{
				SchemaVersion: repositoryStateSchema,
				Snapshot:      test.snapshot,
				State:         testState(1),
			})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if _, err := objects.Put(
				ctx,
				"repos/repo-1/state",
				bytes.NewReader(envelopeJSON),
				int64(len(envelopeJSON)),
				PutOptions{},
			); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			if _, _, err := store.LoadState(ctx, "repo-1"); err == nil || !strings.Contains(err.Error(), "snapshot") {
				t.Fatalf("LoadState() error = %v, want snapshot mismatch", err)
			}
		})
	}
}

func TestRepositoryStore_RejectsOversizedPublication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	objects := NewMemoryStore()
	store, err := NewRepositoryStore(objects)
	if err != nil {
		t.Fatalf("NewRepositoryStore() error = %v", err)
	}
	state := testState(1)
	state.DefaultBranch = "refs/heads/" + strings.Repeat("a", maxRepositoryStateBytes)

	err = store.CompareAndSwapState(ctx, "repo-1", "", state)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("CompareAndSwapState() error = %v, want size error", err)
	}
	_, snapshotName, marshalErr := marshalStateSnapshot(state)
	if marshalErr != nil {
		t.Fatalf("marshalStateSnapshot() error = %v", marshalErr)
	}
	for _, key := range []string{
		"repos/repo-1/state",
		"repos/repo-1/" + snapshotName,
	} {
		if _, err := objects.Head(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Head(%q) error = %v, want ErrNotFound", key, err)
		}
	}
}

func TestValidateState_DefaultBranch(t *testing.T) {
	t.Parallel()

	valid := []string{
		"refs/heads/main",
		"refs/heads/feature/topic",
		"refs/heads/release.v1",
		"refs/heads/-dash",
		"refs/heads/fix-\u00e9",
	}
	for _, branch := range valid {
		state := testState(1)
		state.DefaultBranch = branch
		if err := validateState("repo-1", state); err != nil {
			t.Errorf("validateState(%q) error = %v", branch, err)
		}
	}

	invalid := []string{
		"refs/heads/",
		"refs/tags/main",
		"refs/heads/../main",
		"refs/heads/.hidden",
		"refs/heads/topic.lock",
		"refs/heads/feature..part",
		"refs/heads/feature@{one",
		"refs/heads/with space",
		`refs/heads/with\slash`,
		"refs/heads/control\x00",
		"refs/heads/trailing.",
		"refs/heads/double//slash",
	}
	for _, branch := range invalid {
		state := testState(1)
		state.DefaultBranch = branch
		if err := validateState("repo-1", state); err == nil {
			t.Errorf("validateState(%q) error = nil, want rejection", branch)
		}
	}
}

func TestValidateState_RejectsInvalidStateKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RepositoryState)
	}{
		{
			name: "refs traversal",
			mutate: func(state *RepositoryState) {
				state.RefsSnapshot = "states/../refs.json"
			},
		},
		{
			name: "refs empty component",
			mutate: func(state *RepositoryState) {
				state.RefsSnapshot = "states//refs.json"
			},
		},
		{
			name: "refs backslash",
			mutate: func(state *RepositoryState) {
				state.RefsSnapshot = `states/refs\snapshot.json`
			},
		},
		{
			name: "refs nul",
			mutate: func(state *RepositoryState) {
				state.RefsSnapshot = "states/refs\x00snapshot.json"
			},
		},
		{
			name: "manifest control",
			mutate: func(state *RepositoryState) {
				state.PackManifest = "states/packs\nmanifest.json"
			},
		},
		{
			name: "manifest backslash",
			mutate: func(state *RepositoryState) {
				state.PackManifest = `states/packs\manifest.json`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state := testState(1)
			test.mutate(&state)
			if err := validateState("repo-1", state); err == nil {
				t.Fatal("validateState() error = nil, want invalid key rejection")
			}
		})
	}
}

func putStateArtifacts(
	t *testing.T,
	ctx context.Context,
	store *Store,
	repositoryID string,
	generation uint64,
) {
	t.Helper()

	for _, suffix := range []string{"refs", "packs"} {
		key := "repos/" + repositoryID + "/states/" + generationString(generation) + "-" + suffix + ".json"
		if err := store.PutImmutable(ctx, key, bytes.NewBufferString("{}"), 2); err != nil {
			t.Fatalf("PutImmutable(%q) error = %v", key, err)
		}
	}
}

func testState(generation uint64) RepositoryState {
	name := generationString(generation)
	return RepositoryState{
		SchemaVersion: repositoryStateSchema,
		Generation:    generation,
		DefaultBranch: "refs/heads/main",
		RefsSnapshot:  "states/" + name + "-refs.json",
		PackManifest:  "states/" + name + "-packs.json",
	}
}

func generationString(generation uint64) string {
	return strconv.FormatUint(generation, 10)
}
