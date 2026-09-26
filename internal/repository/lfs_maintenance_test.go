package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func lfsPointer(oid string, size int64) GitObject {
	return GitObject{Type: "blob", Data: fmt.Appendf(nil, "version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)}
}

func publishLFSTag(t *testing.T, store *Store, object GitObject) error {
	t.Helper()
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	id := GitObjectID(object)
	return store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/tags/lfs", New: id}}, map[string]GitObject{id: object}, allowLFS)
}

func TestLFSPointerPublicationRequiresVerifiedObject(t *testing.T) {
	t.Parallel()
	store, _, _ := lfsFixture(t)
	oid := lfsOID([]byte("hello"))
	if err := publishLFSTag(t, store, lfsPointer(oid, 5)); !errors.Is(err, ErrLFSMissing) {
		t.Fatalf("missing object publication = %v", err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil || len(base.References) != 0 {
		t.Fatalf("missing object changed refs: %+v, %v", base, err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
		t.Fatal(err)
	}
	if err := publishLFSTag(t, store, lfsPointer(oid, 6)); !errors.Is(err, ErrLFSHashMismatch) {
		t.Fatalf("wrong pointer size publication = %v", err)
	}
	if err := publishLFSTag(t, store, lfsPointer(oid, 5)); err != nil {
		t.Fatalf("verified pointer publication = %v", err)
	}
	base, err = store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil || base.original.manifest.LFS == nil || base.original.manifest.LFS.Objects[oid] != 5 {
		t.Fatalf("missing generation index: %+v, %v", base, err)
	}
}

func TestLFSIntegrityRestoreAndHistoricalGCRetention(t *testing.T) {
	t.Parallel()
	store, objects, _ := lfsFixture(t)
	oid := lfsOID([]byte("hello"))
	orphan := lfsOID([]byte("orphan"))
	for _, data := range []string{"hello", "orphan"} {
		if _, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte(data)), int64(len(data)), strings.NewReader(data), lfsLimits(), allowLFS); err != nil {
			t.Fatal(err)
		}
	}
	if err := publishLFSTag(t, store, lfsPointer(oid, 5)); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	source, err := maintenanceStateKey(base.original.state)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.CheckIntegrity(t.Context(), "alice", "demo")
	if err != nil || report.LFSObjects != 1 || report.LFSBytes != 5 {
		t.Fatalf("LFS integrity = %+v, %v", report, err)
	}
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/tags/lfs", Old: base.References["refs/tags/lfs"]}}, nil, allowLFS); err != nil {
		t.Fatal(err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != 2 || gc.RetainedGenerations != 3 {
		t.Fatalf("LFS collection = %+v, %v", gc, err)
	}
	if _, err := store.LFSStat(t.Context(), "alice", "demo", orphan); !errors.Is(err, ErrLFSMissing) {
		t.Fatalf("orphan retained: %v", err)
	}
	if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); err != nil {
		t.Fatalf("historical pointer object deleted: %v", err)
	}
	if _, err := store.RestoreGeneration(t.Context(), "alice", "demo", source); err != nil {
		t.Fatalf("LFS generation restore = %v", err)
	}
	record, err := store.readLFSRecord(t.Context(), base.original.metadata.ID, oid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(t.Context(), "repos/"+base.original.metadata.ID+"/"+record.Key, strings.NewReader("wrong"), 5, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "demo"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt LFS integrity = %v", err)
	}
	if _, err := store.RestoreGeneration(t.Context(), "alice", "demo", source); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt LFS restore = %v", err)
	}
	if _, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt LFS GC = %v", err)
	}
}

func TestLFSLegacyGenerationDerivesPointersBeforeGC(t *testing.T) {
	t.Parallel()
	store, _, metadata := lfsFixture(t)
	oid := lfsOID([]byte("hello"))
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	manifest := objectManifest{SchemaVersion: 1, ObjectFormat: "sha1", Objects: map[string]objectInfo{}}
	id, err := store.putObject(t.Context(), metadata.ID, "blob", lfsPointer(oid, 5).Data, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestKey, err := store.snapshotKey(t.Context(), metadata.ID, "manifest", manifest)
	if err != nil {
		t.Fatal(err)
	}
	refsKey, err := store.snapshotKey(t.Context(), metadata.ID, "refs", refsSnapshot{SchemaVersion: 1, Refs: map[string]string{"refs/tags/legacy": id}})
	if err != nil {
		t.Fatal(err)
	}
	next := base.original.state
	next.Generation++
	next.PackManifest, next.RefsSnapshot = manifestKey, refsKey
	if err := store.repositories.CompareAndSwapState(t.Context(), metadata.ID, base.original.version, next); err != nil {
		t.Fatal(err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != 0 {
		t.Fatalf("legacy collection = %+v, %v", gc, err)
	}
	if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); err != nil {
		t.Fatalf("legacy object deleted: %v", err)
	}
}

func TestLFSExpiredReservationFencesPausedFinalization(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	oid := lfsOID([]byte("hello"))
	reservation, key, version, _, err := store.reserveLFS(t.Context(), metadata.ID, LFSObject{OID: oid, Size: 5}, lfsLimits(), objects)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := streamLFSParts(t.Context(), objects, "repos/"+metadata.ID+"/"+reservation.Key, reservation.UploadID, oid, 5, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	reservation.CreatedAt = time.Now().Add(-2 * LFSReservationLifetime)
	reservation.ExpiresAt = reservation.CreatedAt.Add(LFSReservationLifetime)
	info, err := store.putLFSJSON(t.Context(), key, reservation, storage.PutOptions{IfMatch: version})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != 1 {
		t.Fatalf("expired reservation GC = %+v, %v", gc, err)
	}
	if _, _, err := store.finishLFS(t.Context(), metadata.ID, key, info.Version, reservation, parts, objects, allowLFS); !errors.Is(err, ErrConflict) {
		t.Fatalf("paused upload finalized = %v", err)
	}
	if _, err := objects.CompleteMultipart(t.Context(), "repos/"+metadata.ID+"/"+reservation.Key, reservation.UploadID, parts); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired multipart not aborted = %v", err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("expired reservation retained quota: %v", err)
	}
}
