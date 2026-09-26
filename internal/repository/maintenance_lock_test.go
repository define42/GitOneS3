package repository

import (
	"context"
	"errors"
	"testing"
)

func TestRepositoryLockFencesCollectionAndPushAndSurvivesCancellation(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", true))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGit(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	unlock, err := store.lockRepository(ctx, metadata.ID)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if _, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true}); !errors.Is(err, ErrMaintenanceBusy) {
		t.Errorf("collection ignored active writer: %v", err)
	}
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/tags/v1", New: base.References["refs/heads/main"]}}, nil, func(context.Context) error { return nil }); !errors.Is(err, ErrMaintenanceBusy) {
		t.Errorf("push ignored active collection: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetMaintenanceLock(t.Context(), "alice", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lock survived request cancellation cleanup: %v", err)
	}
}

func TestUnlockMaintenanceRequiresOfflineAndMatchingToken(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
	if err != nil {
		t.Fatal(err)
	}
	// Model a terminated process: the returned release function is intentionally
	// not invoked; only explicit operator recovery can remove its durable lock.
	if _, err := store.lockRepository(t.Context(), metadata.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := store.GetMaintenanceLock(t.Context(), "alice", "demo")
	if err != nil || lock.Token == "" || lock.CreatedAt.IsZero() {
		t.Fatalf("GetMaintenanceLock = %+v, %v", lock, err)
	}
	if err := store.UnlockMaintenance(t.Context(), "alice", "demo", lock.Token, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("unlock accepted online operation: %v", err)
	}
	if err := store.UnlockMaintenance(t.Context(), "alice", "demo", "00000000000000000000000000000000", true); !errors.Is(err, ErrConflict) {
		t.Errorf("unlock accepted wrong token: %v", err)
	}
	if err := store.UnlockMaintenance(t.Context(), "alice", "demo", lock.Token, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetMaintenanceLock(t.Context(), "alice", "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recovered lock remained: %v", err)
	}
}
