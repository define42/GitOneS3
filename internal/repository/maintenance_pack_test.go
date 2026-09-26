package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestCheckIntegrityPackedLargeObjectAndTrailerCorruption(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	large := GitObject{Type: "blob", Data: bytes.Repeat([]byte("large packed blob\n"), (maxObjectBytes/18)+100)}
	id := GitObjectID(large)
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/tags/large", New: id}}, map[string]GitObject{id: large}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	report, err := store.CheckIntegrity(t.Context(), "alice", "demo")
	if err != nil || report.Packs != 1 || report.Bytes != int64(len(large.Data)) {
		t.Fatalf("packed integrity = %+v, %v", report, err)
	}
	packed, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	key := "repos/" + metadata.ID + "/" + packed.original.manifest.Objects[id].PackKey
	body, _, err := objects.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	// Corrupt only the trailer, leaving each independently indexed object
	// readable. A full pack integrity check must still detect this damage.
	data[len(data)-1] ^= 1
	if _, err := objects.Put(t.Context(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("pack trailer corruption accepted: %v", err)
	}
	// Even a file with a matching SHA-256 name must have a valid Git trailer.
	digest := sha256.Sum256(data)
	relative := "packs/" + hex.EncodeToString(digest[:]) + ".pack"
	if _, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/"+relative, bytes.NewReader(data), int64(len(data)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.verifyMaintenancePack(t.Context(), metadata.ID, relative, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("invalid Git trailer with matching SHA-256 name accepted: %v", err)
	}
}

func TestGarbageCollectRetainsLegacyObjectsAfterRepack(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Repack(t.Context(), "alice", "demo"); err != nil {
		t.Fatal(err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.Candidates != 0 || report.RetainedGenerations != 2 {
		t.Fatalf("GC after repack = %+v, %v", report, err)
	}
	if _, err := store.LoadGitObjects(t.Context(), pinned); err != nil {
		t.Fatalf("repack and GC invalidated pinned loose generation: %v", err)
	}
	integrity, err := store.CheckIntegrity(t.Context(), "alice", "demo")
	if err != nil || integrity.Packs != 1 || integrity.Objects != 3 {
		t.Fatalf("repacked integrity = %+v, %v", integrity, err)
	}
}
