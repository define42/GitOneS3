package repository

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestCheckIntegrity(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.CheckIntegrity(t.Context(), "alice", "demo")
	if err != nil || report.Objects != 3 || report.References != 1 || report.Generation != 1 || report.Bytes == 0 {
		t.Fatalf("CheckIntegrity = %+v, %v", report, err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	id := base.References["refs/heads/main"]
	if _, err := objects.Put(t.Context(), objectKey(metadata.ID, id), strings.NewReader("damaged"), 7, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged object error = %v, want corruption", err)
	}
}

func TestRestoreGenerationPreservesHistoryAndRecoversDamagedCurrentManifest(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	generations, err := store.ListGenerations(t.Context(), "alice", "demo")
	if err != nil || len(generations) != 1 || !generations[0].Current {
		t.Fatalf("initial generations = %+v, %v", generations, err)
	}
	source := generations[0].Snapshot
	base, err := store.ReadGit(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", Old: base.References["refs/heads/main"]}}, nil, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.repositories.LoadState(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/"+current.PackManifest, strings.NewReader("damaged"), 7, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadGit(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged manifest error = %v", err)
	}
	restored, err := store.RestoreGeneration(t.Context(), "alice", "demo", source)
	if err != nil || restored.Generation != 3 || restored.SourceGeneration != 1 {
		t.Fatalf("RestoreGeneration = %+v, %v", restored, err)
	}
	read, err := store.ReadGit(t.Context(), "alice", "demo")
	if err != nil || read.References["refs/heads/main"] != base.References["refs/heads/main"] {
		t.Fatalf("restored refs = %+v, %v", read, err)
	}
	generations, err = store.ListGenerations(t.Context(), "alice", "demo")
	if err != nil || len(generations) != 3 || !generations[2].Current || generations[0].Current {
		t.Fatalf("restored history = %+v, %v", generations, err)
	}
}

func TestRestoreGenerationRejectsCorruptSourceAndTraversal(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	generations, err := store.ListGenerations(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreGeneration(t.Context(), "alice", "demo", "../other/state"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("traversal error = %v", err)
	}
	if _, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/"+generations[0].Snapshot, strings.NewReader("{}"), 2, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreGeneration(t.Context(), "alice", "demo", generations[0].Snapshot); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt source error = %v", err)
	}
	state, _, err := store.repositories.LoadState(t.Context(), metadata.ID)
	if err != nil || state.Generation != 1 {
		t.Fatalf("failed restore changed publication = %+v, %v", state, err)
	}
}

func TestGarbageCollectDryRunGraceAndApply(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                string
		options             GCOptions
		candidates, deleted int
	}{
		{"default grace", GCOptions{}, 0, 0},
		{"dry run", GCOptions{GracePeriod: time.Nanosecond}, 1, 0},
		{"apply", GCOptions{Apply: true, GracePeriod: time.Nanosecond}, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, objects := newTestStore(t)
			metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
			if err != nil {
				t.Fatal(err)
			}
			manifest := objectManifest{Objects: map[string]objectInfo{}}
			id, err := store.putObject(t.Context(), metadata.ID, "blob", []byte("rejected push data"), &manifest)
			if err != nil {
				t.Fatal(err)
			}
			report, err := store.GarbageCollect(t.Context(), "alice", "demo", tt.options)
			if err != nil || report.Candidates != tt.candidates || report.Deleted != tt.deleted || report.RetainedGenerations != 1 || report.DryRun == tt.options.Apply {
				t.Fatalf("GarbageCollect = %+v, %v", report, err)
			}
			_, err = objects.Head(t.Context(), objectKey(metadata.ID, id))
			if tt.deleted == 1 && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("orphan still present: %v", err)
			}
			if tt.deleted == 0 && err != nil {
				t.Fatalf("orphan removed by dry run or grace period: %v", err)
			}
			if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); err != nil {
				t.Fatalf("collection damaged repository: %v", err)
			}
		})
	}
}

func TestGarbageCollectRetainsDeletedBranchForPinnedReadersAndRestore(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.LoadGitObjects(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	source, err := maintenanceStateKey(base.original.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", Old: base.References["refs/heads/main"]}}, nil, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.RetainedGenerations != 2 || report.Candidates != 0 {
		t.Fatalf("GarbageCollect = %+v, %v", report, err)
	}
	loaded, err := store.LoadGitObjects(t.Context(), pinned)
	if err != nil || len(loaded.Objects) != 3 {
		t.Fatalf("pinned reader lost old generation = %+v, %v", loaded, err)
	}
	if _, err := store.RestoreGeneration(t.Context(), "alice", "demo", source); err != nil {
		t.Fatalf("retained generation not restorable: %v", err)
	}
}

func TestGarbageCollectFailsClosedBeforeDeletingOnCorruption(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	manifest := objectManifest{Objects: map[string]objectInfo{}}
	id, err := store.putObject(t.Context(), metadata.ID, "blob", []byte("orphan"), &manifest)
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(t.Context(), objectKey(metadata.ID, base.References["refs/heads/main"]), ""); err != nil {
		t.Fatal(err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if !errors.Is(err, ErrCorrupt) || report.Deleted != 0 {
		t.Fatalf("corrupt collection = %+v, %v", report, err)
	}
	if _, err := objects.Head(t.Context(), objectKey(metadata.ID, id)); err != nil {
		t.Fatalf("orphan deleted despite corruption: %v", err)
	}
}

func TestGarbageCollectUsesConditionalDelete(t *testing.T) {
	t.Parallel()
	memory := storage.NewMemoryStore()
	objects := &replaceOnDeleteStore{MemoryStore: memory}
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	manifest := objectManifest{Objects: map[string]objectInfo{}}
	id, err := store.putObject(t.Context(), metadata.ID, "blob", []byte("old orphan"), &manifest)
	if err != nil {
		t.Fatal(err)
	}
	objects.target = objectKey(metadata.ID, id)
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if !errors.Is(err, storage.ErrPreconditionFailed) || report.Deleted != 0 {
		t.Fatalf("changed orphan collection = %+v, %v", report, err)
	}
	body, _, err := memory.Get(t.Context(), objects.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

type replaceOnDeleteStore struct {
	*storage.MemoryStore
	target string
}

func (s *replaceOnDeleteStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if key == s.target {
		if _, err := s.Put(ctx, key, bytes.NewReader([]byte("new content")), 11, storage.PutOptions{}); err != nil {
			return err
		}
	}
	return s.MemoryStore.Delete(ctx, key, version)
}

func TestGarbageCollectReclaimsRejectedPushArtifacts(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	incoming := GitObject{Type: "blob", Data: []byte("data from a rejected push")}
	id := GitObjectID(incoming)
	err = store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/tags/rejected", New: id}}, map[string]GitObject{id: incoming}, func(context.Context) error { return errors.New("membership revoked") })
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("push error = %v, want forbidden", err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.Deleted != 3 || report.RetainedGenerations != 1 {
		t.Fatalf("rejected push collection = %+v, %v", report, err)
	}
	current, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil || current.original.state.Generation != 1 || current.References["refs/tags/rejected"] != "" {
		t.Fatalf("rejected push changed repository: %+v, %v", current, err)
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); err != nil {
		t.Fatalf("collection damaged published objects: %v", err)
	}
}

func TestGarbageCollectAcrossListingPages(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
	if err != nil {
		t.Fatal(err)
	}
	const orphanCount = storage.MaxListPageSize + 1
	for i := range orphanCount {
		id := GitObjectID(GitObject{Type: "blob", Data: []byte(time.Unix(int64(i), 0).String())})
		if _, err := objects.Put(t.Context(), objectKey(metadata.ID, id), bytes.NewReader([]byte{'x'}), 1, storage.PutOptions{IfNoneMatch: true}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.Deleted != orphanCount || report.CandidateBytes != orphanCount {
		t.Fatalf("paginated collection = %+v, %v", report, err)
	}
}
