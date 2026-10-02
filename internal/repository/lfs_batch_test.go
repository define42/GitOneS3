package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestLFSStatBatchStorageLatency(t *testing.T) {
	t.Parallel()
	store, _, oids := lfsBatchFixture(t, maxLFSBatchObjects)
	synctest.Test(t, func(t *testing.T) {
		observed := &lfsBatchReadStore{ObjectStore: store.objects, delay: 50 * time.Millisecond}
		store.objects = observed
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		started := time.Now()
		results, err := store.LFSStatBatch(ctx, "alice", "demo", oids)
		if err != nil || len(results) != len(oids) {
			t.Fatalf("batch = %d results, %v", len(results), err)
		}
		for i, result := range results {
			if result.Err != nil || result.Object.OID != oids[i] || result.Object.Size != int64(len(fmt.Sprintf("object %d", i))) {
				t.Fatalf("result %d = %+v", i, result)
			}
		}
		if observed.metadataReads.Load() != 1 || observed.recordReads.Load() != maxLFSBatchObjects || observed.heads.Load() != maxLFSBatchObjects {
			t.Fatalf("reads: metadata = %d, records = %d, heads = %d", observed.metadataReads.Load(), observed.recordReads.Load(), observed.heads.Load())
		}
		if observed.peak.Load() != maxLFSBatchReads || observed.active.Load() != 0 {
			t.Fatalf("workers: peak = %d, active = %d", observed.peak.Load(), observed.active.Load())
		}
		if elapsed := time.Since(started); elapsed > 13*time.Second {
			t.Fatalf("1000 objects with 50ms storage latency took %s", elapsed)
		}
	})
}

func TestLFSStatBatchErrorsAndDuplicates(t *testing.T) {
	t.Parallel()
	store, metadata, oids := lfsBatchFixture(t, 3)
	// Overwriting an otherwise intact object changes its storage version and
	// must invalidate its verified record, including within a batch.
	key := "repos/" + metadata.ID + "/lfs/objects/" + fmt.Sprintf("%032x", 1)
	if _, err := store.objects.Put(t.Context(), key, strings.NewReader("object 1"), 8, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	observed := &lfsBatchReadStore{ObjectStore: store.objects}
	store.objects = observed
	missing := strings.Repeat("f", 64)
	requested := []string{oids[2], missing, "invalid", oids[1], oids[0], oids[2]}
	results, err := store.LFSStatBatch(t.Context(), "alice", "demo", requested)
	if err != nil || len(results) != len(requested) {
		t.Fatalf("batch = %+v, %v", results, err)
	}
	for i, want := range []error{nil, ErrLFSMissing, ErrInvalid, ErrCorrupt, nil, nil} {
		if !errors.Is(results[i].Err, want) || (want == nil && results[i].Object.OID != requested[i]) {
			t.Errorf("result %d = %+v, want %v", i, results[i], want)
		}
	}
	if !errors.Is(results[1].Err, ErrNotFound) || observed.recordReads.Load() != 4 || observed.heads.Load() != 3 {
		t.Fatalf("missing error = %v, record reads = %d, heads = %d", results[1].Err, observed.recordReads.Load(), observed.heads.Load())
	}
	if _, err := store.Create(t.Context(), "bob", createInput("demo", false)); err != nil {
		t.Fatal(err)
	}
	results, err = store.LFSStatBatch(t.Context(), "bob", "demo", []string{oids[0]})
	if err != nil || len(results) != 1 || !errors.Is(results[0].Err, ErrLFSMissing) {
		t.Fatalf("cross-repository lookup = %+v, %v", results, err)
	}
	// Metadata is validated again on the next call; it is never cached as an
	// authority to resolve a repository name to an object prefix.
	if _, err := store.objects.Put(t.Context(), metadataKey("alice", "demo"), strings.NewReader("{}"), 2, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	results, err = store.LFSStatBatch(t.Context(), "alice", "demo", []string{oids[0], "invalid"})
	if err != nil || len(results) != 2 || !errors.Is(results[0].Err, ErrCorrupt) || !errors.Is(results[1].Err, ErrInvalid) {
		t.Fatalf("invalid metadata = %+v, %v", results, err)
	}
}

func TestLFSStatBatchBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	store, _, oids := lfsBatchFixture(t, maxLFSBatchObjects)
	observed := &lfsBatchReadStore{ObjectStore: store.objects}
	store.objects = observed
	if _, err := store.LFSStatBatch(t.Context(), "alice", "demo", append(slices.Clone(oids), oids[0])); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversized batch = %v", err)
	}
	if results, err := store.LFSStatBatch(t.Context(), "alice", "demo", nil); err != nil || len(results) != 0 {
		t.Fatalf("empty batch = %+v, %v", results, err)
	}
	if observed.metadataReads.Load() != 0 || observed.recordReads.Load() != 0 || observed.heads.Load() != 0 {
		t.Fatal("empty or oversized batch accessed storage")
	}
	synctest.Test(t, func(t *testing.T) {
		observed.delay = 50 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 125*time.Millisecond)
		defer cancel()
		results, err := store.LFSStatBatch(ctx, "alice", "demo", oids)
		if !errors.Is(err, context.DeadlineExceeded) || results != nil {
			t.Fatalf("cancelled batch = %+v, %v", results, err)
		}
		if observed.active.Load() != 0 || observed.peak.Load() != maxLFSBatchReads || observed.recordReads.Load() != maxLFSBatchReads || observed.heads.Load() != maxLFSBatchReads {
			t.Fatalf("cancellation: active = %d, peak = %d, records = %d, heads = %d", observed.active.Load(), observed.peak.Load(), observed.recordReads.Load(), observed.heads.Load())
		}
	})
}

func lfsBatchFixture(t *testing.T, count int) (*Store, Metadata, []string) {
	t.Helper()
	store, objects, metadata := lfsFixture(t)
	oids := make([]string, count)
	for i := range count {
		data := []byte(fmt.Sprintf("object %d", i))
		oids[i] = lfsOID(data)
		key := "lfs/objects/" + fmt.Sprintf("%032x", i)
		info, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/"+key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		record := lfsRecord{SchemaVersion: 1, Object: LFSObject{OID: oids[i], Size: int64(len(data))}, Key: key, Version: info.Version}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := objects.Put(t.Context(), lfsRecordKey(metadata.ID, oids[i]), bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return store, metadata, oids
}

type lfsBatchReadStore struct {
	storage.ObjectStore
	delay         time.Duration
	metadataReads atomic.Int64
	recordReads   atomic.Int64
	heads         atomic.Int64
	active        atomic.Int64
	peak          atomic.Int64
}

func (s *lfsBatchReadStore) wait(ctx context.Context) error {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for peak := s.peak.Load(); active > peak; peak = s.peak.Load() {
		if s.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if s.delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *lfsBatchReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.HasPrefix(key, "repositories/") {
		s.metadataReads.Add(1)
	} else if strings.Contains(key, "/lfs/verified/") {
		s.recordReads.Add(1)
	}
	if err := s.wait(ctx); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *lfsBatchReadStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.heads.Add(1)
	if err := s.wait(ctx); err != nil {
		return storage.ObjectInfo{}, err
	}
	return s.ObjectStore.Head(ctx, key)
}
