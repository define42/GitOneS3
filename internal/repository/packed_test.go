package repository

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/storage"
)

func packedWorkspace(t *testing.T, ids []string, resolve gitpack.Resolver) *gitpack.Workspace {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "incoming.pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, f)
	if _, err := gitpack.Write(t.Context(), f, ids, resolve, PackLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	w, err := gitpack.Decode(t.Context(), bufio.NewReader(f), PackLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return w
}

func TestPublishPackSupportsLargeRepositoriesAndIndividualObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("80 MiB repository regression")
	}
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("large", false)); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "large")
	if err != nil {
		t.Fatal(err)
	}
	seeds := map[string]byte{}
	metadata := map[string]gitpack.Object{}
	var ids []string
	var tree bytes.Buffer
	for i := byte(1); i <= 5; i++ {
		id := GitObjectID(GitObject{Type: "blob", Data: bytes.Repeat([]byte{i}, MaxGitObjectBytes)})
		seeds[id] = i
		ids = append(ids, id)
		fmt.Fprintf(&tree, "100644 large-%d.bin\x00", i)
		rawID, err := hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		tree.Write(rawID)
	}
	readme := GitObject{Type: "blob", Data: []byte("# Large repository\n")}
	readmeID := GitObjectID(readme)
	metadata[readmeID] = gitpack.Object{Type: readme.Type, Data: readme.Data}
	ids = append(ids, readmeID)
	rawID, err := hex.DecodeString(readmeID)
	if err != nil {
		t.Fatal(err)
	}
	// Native Git tree order places uppercase README before the other entries.
	treeData := append([]byte("100644 README.md\x00"), rawID...)
	treeData = append(treeData, tree.Bytes()...)
	treeID := GitObjectID(GitObject{Type: "tree", Data: treeData})
	metadata[treeID] = gitpack.Object{Type: "tree", Data: treeData}
	ids = append(ids, treeID)
	commit := GitObject{Type: "commit", Data: []byte("tree " + treeID + "\nauthor Test <test@example.test> 1700000000 +0000\ncommitter Test <test@example.test> 1700000000 +0000\n\nLarge repository\n")}
	commitID := GitObjectID(commit)
	metadata[commitID] = gitpack.Object{Type: commit.Type, Data: commit.Data}
	ids = append(ids, commitID)
	resolve := func(ctx context.Context, id string) (gitpack.Object, error) {
		if seed, ok := seeds[id]; ok {
			return gitpack.Object{Type: "blob", Data: bytes.Repeat([]byte{seed}, MaxGitObjectBytes)}, ctx.Err()
		}
		return metadata[id], ctx.Err()
	}
	incoming := packedWorkspace(t, ids, resolve)
	if err := store.PublishPack(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", New: commitID}}, incoming, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, err := store.ReadGitReferences(t.Context(), "alice", "large")
	if err != nil {
		t.Fatal(err)
	}
	if current.Objects != nil || current.original.manifest.SchemaVersion != 2 || len(current.original.manifest.Objects) != 8 {
		t.Fatalf("invalid packed snapshot: %+v", current.original.manifest)
	}
	for _, info := range current.original.manifest.Objects {
		if info.PackKey == "" {
			t.Fatal("new object was stored loose")
		}
	}
	if _, err := store.LoadGitObjects(t.Context(), current); !errors.Is(err, ErrLimit) {
		t.Fatalf("materialization must remain bounded: %v", err)
	}
	reader, err := store.OpenGit(t.Context(), current)
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, reader)
	if err := reader.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	reachable, err := reader.Reachable(t.Context(), current.References)
	if err != nil || len(reachable) != 8 {
		t.Fatalf("reachable: %v %v", reachable, err)
	}
	got, err := reader.Get(t.Context(), ids[0])
	if err != nil || len(got.Data) != MaxGitObjectBytes || got.Data[0] != 1 {
		t.Fatalf("16 MiB packed blob size=%d error=%v", len(got.Data), err)
	}
	blob, err := store.Blob(t.Context(), "alice", "large", "main", "README.md")
	if err != nil || blob.Content != string(readme.Data) {
		t.Fatalf("packed browser read: %+v %v", blob, err)
	}
	if _, err := store.Blob(t.Context(), "alice", "large", "main", "large-1.bin"); !errors.Is(err, ErrLimit) {
		t.Fatalf("browser size bound: %v", err)
	}
	report, err := store.CheckIntegrity(t.Context(), "alice", "large")
	if err != nil || report.Bytes <= 80<<20 || report.Objects != 8 || report.Packs != 1 {
		t.Fatalf("large integrity report: %+v %v", report, err)
	}
}

type packReadStore struct {
	*storage.MemoryStore
	ranges int
	packs  int
}

func (s *packReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/packs/") {
		s.packs++
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *packReadStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/packs/") {
		s.ranges++
	}
	return s.MemoryStore.GetRange(ctx, key, offset, length)
}

func TestRepackMigratesLegacyAndBrowserUsesRanges(t *testing.T) {
	objects := &packReadStore{MemoryStore: storage.NewMemoryStore()}
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.original.manifest.SchemaVersion != 1 {
		t.Fatal("expected legacy fixture")
	}
	if err := store.Repack(t.Context(), "alice", "demo"); err != nil {
		t.Fatal(err)
	}
	packed, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if packed.original.manifest.SchemaVersion != 2 || packed.original.state.Generation != 2 {
		t.Fatal("repack did not publish format 2")
	}
	objects.packs, objects.ranges = 0, 0
	blob, err := store.Blob(t.Context(), "alice", "demo", "main", "README.md")
	if err != nil || !strings.HasPrefix(blob.Content, "# demo") {
		t.Fatalf("browser read: %+v %v", blob, err)
	}
	if objects.packs != 0 || objects.ranges != 3 {
		t.Fatalf("browser loaded full packs: packs=%d ranges=%d", objects.packs, objects.ranges)
	}
	// Identical content must safely reuse an existing canonical pack.
	if err := store.Repack(t.Context(), "alice", "demo"); err != nil {
		t.Fatal(err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.RetainedGenerations != 3 {
		t.Fatalf("GC retained history: %+v %v", gc, err)
	}
	reader, err := store.OpenGit(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer closePackedResource(t, reader)
	if err := reader.Validate(t.Context()); err != nil {
		t.Fatalf("GC broke legacy pinned generation: %v", err)
	}
	if _, err := reader.Get(t.Context(), legacy.References["refs/heads/main"]); err != nil {
		t.Fatalf("legacy object removed: %v", err)
	}
	if report, err := store.CheckIntegrity(t.Context(), "alice", "demo"); err != nil || report.Packs != 1 {
		t.Fatalf("packed integrity: %+v %v", report, err)
	}
}

func TestRejectedPackIsCollectedWithoutChangingPublishedHistory(t *testing.T) {
	store, base := transportFixture(t)
	object := gitpack.Object{Type: "blob", Data: []byte("rejected object content")}
	id := GitObjectID(GitObject{Type: object.Type, Data: object.Data})
	incoming := packedWorkspace(t, []string{id}, func(context.Context, string) (gitpack.Object, error) { return object, nil })
	checked := false
	err := store.PublishPack(t.Context(), base, []RefUpdate{{Name: "refs/tags/rejected", New: id}}, incoming, func(ctx context.Context) error {
		checked = true
		packs, err := store.objects.List(ctx, "repos/"+base.original.metadata.ID+"/packs/")
		if err != nil {
			return err
		}
		if len(packs) != 1 {
			t.Errorf("authorization ran before pack was durable: %d", len(packs))
		}
		return errors.New("token revoked")
	})
	if !checked || !errors.Is(err, ErrForbidden) {
		t.Fatalf("rejected pack: %v", err)
	}
	current, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil || current.original.version != base.original.version {
		t.Fatalf("rejected pack changed generation: %v", err)
	}
	preview, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{GracePeriod: time.Nanosecond})
	if err != nil || preview.Candidates < 3 || preview.Deleted != 0 {
		t.Fatalf("orphan preview: %+v %v", preview, err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != preview.Candidates {
		t.Fatalf("orphan collection: %+v %v", gc, err)
	}
	packs, err := store.objects.List(t.Context(), "repos/"+base.original.metadata.ID+"/packs/")
	if err != nil || len(packs) != 0 {
		t.Fatalf("rejected pack survived: %v %v", packs, err)
	}
	if _, err := store.Blob(t.Context(), "alice", "demo", "main", "README.md"); err != nil {
		t.Fatalf("published history damaged: %v", err)
	}
	// A stale reference snapshot is rejected before uploading a replacement pack.
	if err := store.PublishGit(t.Context(), current, []RefUpdate{{Name: "refs/tags/live", New: current.References["refs/heads/main"]}}, nil, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPack(t.Context(), base, []RefUpdate{{Name: "refs/tags/stale", New: id}}, incoming, func(context.Context) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale pack publication: %v", err)
	}
	packs, err = store.objects.List(t.Context(), "repos/"+base.original.metadata.ID+"/packs/")
	if err != nil || len(packs) != 0 {
		t.Fatalf("stale push uploaded pack: %v %v", packs, err)
	}
}

func TestPackedCorruptionAndManifestValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*objectInfo)
	}{
		{"path traversal", func(i *objectInfo) { i.PackKey = "packs/../../other.pack" }},
		{"negative offset", func(i *objectInfo) { i.Offset = -1 }},
		{"trailer overlap", func(i *objectInfo) { i.Offset = MaxPackBytes - 20 }},
		{"oversized object", func(i *objectInfo) { i.Size = MaxGitObjectBytes + 1 }},
		{"oversized entry", func(i *objectInfo) { i.Length = MaxPackBytes }},
		{"invalid strong digest", func(i *objectInfo) { i.SHA256 = "bad" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := transportFixture(t)
			if err := store.Repack(t.Context(), "alice", "demo"); err != nil {
				t.Fatal(err)
			}
			base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			manifest := base.original.manifest
			manifest.Objects = maps.Clone(manifest.Objects)
			id := base.References["refs/heads/main"]
			info := manifest.Objects[id]
			tt.change(&info)
			manifest.Objects[id] = info
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
			if _, err := store.ReadGitReferences(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("bad packed location accepted: %v", err)
			}
		})
	}
	t.Run("mutated pack bytes", func(t *testing.T) {
		store, _ := transportFixture(t)
		if err := store.Repack(t.Context(), "alice", "demo"); err != nil {
			t.Fatal(err)
		}
		base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
		if err != nil {
			t.Fatal(err)
		}
		id := base.References["refs/heads/main"]
		info := base.original.manifest.Objects[id]
		key := "repos/" + base.original.metadata.ID + "/" + info.PackKey
		body, metadata, err := store.objects.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		if err := errors.Join(err, body.Close()); err != nil {
			t.Fatal(err)
		}
		data[info.Offset+info.Length-1] ^= 1
		if _, err := store.objects.Put(t.Context(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: metadata.Version}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Blob(t.Context(), "alice", "demo", "main", "README.md"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("range corruption accepted: %v", err)
		}
		reader, err := store.OpenGit(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		defer closePackedResource(t, reader)
		if _, err := reader.Get(t.Context(), id); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("pack corruption accepted: %v", err)
		}
		if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("integrity accepted corruption: %v", err)
		}
	})
}

func closePackedResource(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}
