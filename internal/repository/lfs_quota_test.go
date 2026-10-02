package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

type quotaCountingStore struct {
	*storage.MemoryStore
	gets, lists, verifiedGets int
}

func (s *quotaCountingStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.gets++
	if strings.Contains(key, "/lfs/verified/") {
		s.verifiedGets++
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *quotaCountingStore) ListPage(ctx context.Context, prefix, after string, limit int) (storage.ObjectPage, error) {
	s.lists++
	return s.MemoryStore.ListPage(ctx, prefix, after, limit)
}

func initializeQuota(t *testing.T, store *Store, repositoryID string) lfsQuota {
	t.Helper()
	unlock, err := store.lockRepository(t.Context(), repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := store.loadLFSQuota(t.Context(), repositoryID)
	if unlockErr := unlock(); err != nil || unlockErr != nil {
		t.Fatalf("load quota = %+v, %v, unlock = %v", quota, err, unlockErr)
	}
	return quota
}

func seedQuotaHistory(t *testing.T, store *Store, repositoryID string, count int) {
	t.Helper()
	for i := range count {
		data := []byte(fmt.Sprintf("history-%d", i))
		key := fmt.Sprintf("lfs/objects/%032x", i+1)
		info, err := store.objects.Put(t.Context(), "repos/"+repositoryID+"/"+key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true})
		if err != nil {
			t.Fatal(err)
		}
		record := lfsRecord{SchemaVersion: 1, Object: LFSObject{OID: lfsOID(data), Size: int64(len(data))}, Key: key, Version: info.Version}
		if _, err := store.putLFSJSON(t.Context(), lfsRecordKey(repositoryID, record.Object.OID), record, storage.PutOptions{IfNoneMatch: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLFSQuotaUploadStorageReadsDoNotGrowWithHistory(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			seedQuotaHistory(t, store, metadata.ID, count)
			quota := initializeQuota(t, store, metadata.ID)
			if quota.Objects != count {
				t.Fatalf("migration objects = %d, want %d", quota.Objects, count)
			}
			counted := &quotaCountingStore{MemoryStore: objects}
			store.objects = counted
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
				t.Fatal(err)
			}
			if counted.lists != 0 || counted.verifiedGets != 1 || counted.gets > 5 {
				t.Fatalf("history %d: LIST=%d GET=%d verified GET=%d; want 0 LIST, <=5 GET, only incoming verified lookup", count, counted.lists, counted.gets, counted.verifiedGets)
			}
			quota = initializeQuota(t, store, metadata.ID)
			if quota.Objects != count+1 || len(quota.Reservations) != 0 || quota.Dirty {
				t.Fatalf("completed quota = %+v", quota)
			}
		})
	}
}

type quotaFaultStore struct {
	*storage.MemoryStore
	failWrite, writes int
	failWrites        map[int]bool
	after             bool
	failure           error
	cancel            context.CancelFunc
}

func (s *quotaFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.ObjectInfo, error) {
	if !strings.HasSuffix(key, "/lfs/quota.json") {
		return s.MemoryStore.Put(ctx, key, body, size, options)
	}
	s.writes++
	if s.writes != s.failWrite && !s.failWrites[s.writes] {
		return s.MemoryStore.Put(ctx, key, body, size, options)
	}
	if errors.Is(s.failure, storage.ErrPreconditionFailed) {
		// Advance the durable version while retaining the old contents. The
		// attempted update must use its stale version and fail the actual CAS.
		current, info, err := s.Get(ctx, key)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		data, err := io.ReadAll(current)
		if err = errors.Join(err, current.Close()); err != nil {
			return storage.ObjectInfo{}, err
		}
		if _, err := s.MemoryStore.Put(ctx, key, bytes.NewReader(data), info.Size, storage.PutOptions{IfMatch: info.Version}); err != nil {
			return storage.ObjectInfo{}, err
		}
		return s.MemoryStore.Put(ctx, key, body, size, options)
	}
	var info storage.ObjectInfo
	var err error
	if s.after {
		info, err = s.MemoryStore.Put(ctx, key, body, size, options)
	}
	if s.cancel != nil {
		s.cancel()
	}
	return info, errors.Join(s.failure, err)
}

// Assert accounting against physical artifacts, independently of ledger totals.
func assertQuotaMatchesArtifacts(t *testing.T, store *Store, repositoryID string) lfsQuota {
	t.Helper()
	quota := initializeQuota(t, store, repositoryID)
	artifacts, err := store.lfsArtifacts(t.Context(), repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	physical := map[string]int64{}
	var used int64
	count, pending := 0, 0
	for _, info := range artifacts {
		if strings.Contains(info.Key, "/lfs/objects/") {
			physical[info.Key] = info.Size
			used += info.Size
			count++
		}
	}
	for _, info := range artifacts {
		if !strings.Contains(info.Key, "/lfs/uploads/") {
			continue
		}
		reservation, err := store.readLFSReservation(t.Context(), repositoryID, info)
		if err != nil {
			t.Fatal(err)
		}
		pending++
		if _, ok := physical["repos/"+repositoryID+"/"+reservation.Key]; !ok {
			used += reservation.Object.Size
			count++
		}
	}
	if quota.Dirty || quota.Bytes != used || quota.Objects != count || len(quota.Reservations) != pending {
		t.Fatalf("quota=%+v; physical/reserved bytes=%d objects=%d pending=%d", quota, used, count, pending)
	}
	return quota
}

func TestLFSQuotaEachWriteBoundaryPreservesCharges(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"before", "ambiguous", "CAS", "cancellation"} {
		for write := 1; write <= 6; write++ {
			t.Run(fmt.Sprintf("%s/%d", mode, write), func(t *testing.T) {
				store, objects, metadata := lfsFixture(t)
				initializeQuota(t, store, metadata.ID)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fault := &quotaFaultStore{MemoryStore: objects, failWrite: write, failure: errors.New("injected quota write failure")}
				switch mode {
				case "ambiguous":
					fault.after = true
				case "CAS":
					fault.failure = storage.ErrPreconditionFailed
				case "cancellation":
					fault.after, fault.failure, fault.cancel = true, context.Canceled, cancel
				}
				store.objects = fault
				limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
				_, err := store.LFSUpload(ctx, "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), limits, allowLFS)
				if !errors.Is(err, fault.failure) {
					t.Fatalf("write %d: upload error=%v, want %v", write, err, fault.failure)
				}
				store.objects = objects
				quota := assertQuotaMatchesArtifacts(t, store, metadata.ID)
				_, err = store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), limits, allowLFS)
				if quota.Bytes == 5 && !errors.Is(err, ErrLFSQuota) {
					t.Fatalf("charged upload admitted: %v", err)
				}
				if quota.Bytes == 0 && err != nil {
					t.Fatalf("released quota unavailable: %v", err)
				}
				assertQuotaMatchesArtifacts(t, store, metadata.ID)
			})
		}
	}
}

type quotaDeleteFailureStore struct {
	*storage.MemoryStore
	failKey string
	after   bool
}

func (s *quotaDeleteFailureStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if key != s.failKey {
		return s.MemoryStore.Delete(ctx, key, version)
	}
	if s.after {
		if err := s.MemoryStore.Delete(ctx, key, version); err != nil {
			return err
		}
	}
	return errors.New("injected deletion failure")
}

func TestLFSQuotaPartialGCRepairsBeforeAdmission(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", after), func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			oid := lfsOID([]byte("hello"))
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
				t.Fatal(err)
			}
			record, err := store.readLFSRecord(t.Context(), metadata.ID, oid)
			if err != nil {
				t.Fatal(err)
			}
			store.objects = &quotaDeleteFailureStore{MemoryStore: objects, failKey: "repos/" + metadata.ID + "/" + record.Key, after: after}
			if _, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond}); err == nil {
				t.Fatal("GC unexpectedly succeeded")
			}
			quota, err := store.readLFSQuota(t.Context(), metadata.ID)
			if err != nil || !quota.Dirty {
				t.Fatalf("partial GC ledger = %+v, %v", quota, err)
			}
			store.objects = objects
			quota = assertQuotaMatchesArtifacts(t, store, metadata.ID)
			_, err = store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), limits, allowLFS)
			if after && err != nil || !after && !errors.Is(err, ErrLFSQuota) {
				t.Fatalf("repaired bytes=%d admission=%v", quota.Bytes, err)
			}
		})
	}
}

func TestLFSQuotaMigrationBoundCanBeRecoveredByGC(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	created := time.Now().Add(-48 * time.Hour).UTC()
	for i := range maxLFSReservations + 1 {
		token := fmt.Sprintf("%032x", i+1)
		reservation := lfsReservation{SchemaVersion: 1, Object: LFSObject{OID: fmt.Sprintf("%064x", i+1), Size: 1}, Key: "lfs/objects/" + token, CreatedAt: created, ExpiresAt: created.Add(LFSReservationLifetime)}
		if _, err := store.putLFSJSON(t.Context(), "repos/"+metadata.ID+"/lfs/uploads/"+token+".json", reservation, storage.PutOptions{IfNoneMatch: true}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), lfsLimits(), allowLFS)
	if !errors.Is(err, ErrLFSQuota) {
		t.Fatalf("legacy reservation overflow=%v", err)
	}
	if _, err := objects.Head(t.Context(), lfsQuotaKey(metadata.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed migration published a ledger: %v", err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.Deleted != maxLFSReservations+1 {
		t.Fatalf("legacy cleanup=%+v, %v", report, err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
		t.Fatal(err)
	}
	assertQuotaMatchesArtifacts(t, store, metadata.ID)
}

func TestLFSQuotaUnknownSchemaFailsClosed(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	data := `{"schemaVersion":2,"bytes":0,"objects":0,"reservations":{},"dirty":false}`
	if _, err := objects.Put(t.Context(), lfsQuotaKey(metadata.ID), strings.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	_, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), lfsLimits(), allowLFS)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unsupported quota schema admitted upload: %v", err)
	}
	reservations, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/uploads/")
	if err != nil || len(reservations) != 0 {
		t.Fatalf("unsupported quota mutated reservations: %v, %v", reservations, err)
	}
}

func TestLFSQuotaReconcilesCrashArtifacts(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"dirty", "reserved", "completed", "published", "released", "aborted"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			quota := initializeQuota(t, store, metadata.ID)
			unlock, err := store.lockRepository(t.Context(), metadata.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.saveLFSQuota(t.Context(), metadata.ID, &quota, true); err != nil {
				t.Fatal(err)
			}
			token := strings.Repeat("a", 32)
			object := LFSObject{OID: lfsOID([]byte("hello")), Size: 5}
			created := time.Now().UTC()
			reservation := lfsReservation{SchemaVersion: 1, Object: object, Key: "lfs/objects/" + token, CreatedAt: created, ExpiresAt: created.Add(LFSReservationLifetime)}
			key := "repos/" + metadata.ID + "/lfs/uploads/" + token + ".json"
			var pending storage.ObjectInfo
			if stage != "dirty" {
				pending, err = store.putLFSJSON(t.Context(), key, reservation, storage.PutOptions{IfNoneMatch: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "completed" || stage == "published" || stage == "released" {
				info, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/"+reservation.Key, strings.NewReader("hello"), 5, storage.PutOptions{IfNoneMatch: true})
				if err != nil {
					t.Fatal(err)
				}
				if stage != "completed" {
					record := lfsRecord{SchemaVersion: 1, Object: object, Key: reservation.Key, Version: info.Version}
					if _, err := store.putLFSJSON(t.Context(), lfsRecordKey(metadata.ID, object.OID), record, storage.PutOptions{IfNoneMatch: true}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if stage == "released" || stage == "aborted" {
				if err := objects.Delete(t.Context(), key, pending.Version); err != nil {
					t.Fatal(err)
				}
			}
			// Model restart after the operator has fenced the dead writer and
			// removed its durable lock. No normal upload cleanup runs here.
			if err := unlock(); err != nil {
				t.Fatal(err)
			}
			restarted := *store
			quota = assertQuotaMatchesArtifacts(t, &restarted, metadata.ID)
			wantBytes := int64(5)
			if stage == "dirty" || stage == "aborted" {
				wantBytes = 0
			}
			if quota.Bytes != wantBytes {
				t.Fatalf("recovered bytes=%d, want %d", quota.Bytes, wantBytes)
			}
			_, err = restarted.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}, allowLFS)
			if wantBytes == 5 && !errors.Is(err, ErrLFSQuota) || wantBytes == 0 && err != nil {
				t.Fatalf("post-crash admission = %v", err)
			}
		})
	}
}

func TestLFSQuotaSharedAcrossStoreInstances(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	second := *store
	limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
	oid := lfsOID([]byte("hello"))
	reservation, key, version, existing, err := store.reserveLFS(t.Context(), metadata.ID, LFSObject{OID: oid, Size: 5}, limits, objects)
	if err != nil || existing {
		t.Fatalf("first writer reserve = %+v, %v, %v", reservation, existing, err)
	}
	_, err = second.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), limits, allowLFS)
	if !errors.Is(err, ErrLFSQuota) {
		t.Fatalf("second writer ignored first writer's reservation: %v", err)
	}
	_, err = second.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS)
	if !errors.Is(err, ErrLFSQuota) {
		t.Fatalf("second writer did not charge duplicate in-flight OID: %v", err)
	}
	if err := objects.AbortMultipart(t.Context(), "repos/"+metadata.ID+"/"+reservation.Key, reservation.UploadID); err != nil {
		t.Fatal(err)
	}
	if err := store.releaseLFSReservation(t.Context(), metadata.ID, key, version); err != nil {
		t.Fatal(err)
	}
	if _, err := second.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
		t.Fatalf("second writer could not use released quota: %v", err)
	}
}

type quotaCancelReadStore struct {
	*storage.MemoryStore
	cancel context.CancelFunc
}

func (s *quotaCancelReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.MemoryStore.Get(ctx, key)
	if strings.Contains(key, "/lfs/verified/") {
		s.cancel()
	}
	return body, info, err
}

func TestLFSQuotaCancelledReconciliationStaysDirty(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	seedQuotaHistory(t, store, metadata.ID, 1)
	initializeQuota(t, store, metadata.ID)
	unlock, err := store.lockRepository(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unlock(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := store.invalidateLFSQuota(t.Context(), metadata.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.objects = &quotaCancelReadStore{MemoryStore: objects, cancel: cancel}
	if _, err := store.loadLFSQuota(ctx, metadata.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reconciliation = %v", err)
	}
	store.objects = objects
	quota, err := store.readLFSQuota(t.Context(), metadata.ID)
	if err != nil || !quota.Dirty {
		t.Fatalf("canceled repair committed clean quota: %+v, %v", quota, err)
	}
	quota, err = store.loadLFSQuota(t.Context(), metadata.ID)
	if err != nil || quota.Dirty || quota.Objects != 1 {
		t.Fatalf("repair retry = %+v, %v", quota, err)
	}
}

func TestLFSQuotaFailedInitializationCanRetryAtFullQuota(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", after), func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			initializeQuota(t, store, metadata.ID)
			failure := errors.New("lost final initialization result")
			store.objects = &quotaFaultStore{MemoryStore: objects, failWrite: 2, after: after, failure: failure}
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
			oid := lfsOID([]byte("hello"))
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); !errors.Is(err, failure) {
				t.Fatalf("injected upload = %v", err)
			}
			store.objects = objects
			quota, err := store.readLFSQuota(t.Context(), metadata.ID)
			if err != nil || quota.Dirty || quota.Bytes != 0 || quota.Objects != 0 || len(quota.Reservations) != 0 {
				t.Fatalf("initialization cleanup did not restore quota: %+v, %v", quota, err)
			}
			counted := &quotaCountingStore{MemoryStore: objects}
			store.objects = counted
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
				t.Fatalf("same-OID retry at full quota = %v", err)
			}
			if counted.lists != 0 {
				t.Fatalf("retry rescanned repository history %d times", counted.lists)
			}
			assertQuotaMatchesArtifacts(t, store, metadata.ID)
		})
	}
}

func TestLFSQuotaUncertainCleanupKeepsReservationCharged(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", after), func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			initializeQuota(t, store, metadata.ID)
			failure := errors.New("initialization and cleanup writes failed")
			store.objects = &quotaFaultStore{MemoryStore: objects, failWrites: map[int]bool{2: true, 3: true}, after: after, failure: failure}
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 10}
			oid := lfsOID([]byte("hello"))
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); !errors.Is(err, failure) {
				t.Fatalf("injected upload = %v", err)
			}
			store.objects = objects
			quota := assertQuotaMatchesArtifacts(t, store, metadata.ID)
			if quota.Bytes != 5 || len(quota.Reservations) != 1 {
				t.Fatalf("uncertain cleanup released its charge: %+v", quota)
			}
			// An independently charged retry is allowed once storage recovers.
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
				t.Fatalf("same-OID retry with headroom = %v", err)
			}
			quota = assertQuotaMatchesArtifacts(t, store, metadata.ID)
			if quota.Bytes != 10 || quota.Objects != 2 || len(quota.Reservations) != 1 {
				t.Fatalf("retry did not charge both attempts: %+v", quota)
			}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), limits, allowLFS); !errors.Is(err, ErrLFSQuota) {
				t.Fatalf("outstanding initialization charge was bypassed: %v", err)
			}
		})
	}
}

type quotaCleanupFaultStore struct {
	*quotaFaultStore
	abortFailure, deleteFailure, deleteAfter, unexpectedPayload bool
}

func (s *quotaCleanupFaultStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if s.abortFailure {
		return errors.New("abort unavailable")
	}
	if s.unexpectedPayload {
		if _, err := s.MemoryStore.Put(ctx, key, strings.NewReader("hello"), 5, storage.PutOptions{IfNoneMatch: true}); err != nil {
			return err
		}
	}
	return s.MemoryStore.AbortMultipart(ctx, key, uploadID)
}

func (s *quotaCleanupFaultStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if s.deleteFailure && strings.Contains(key, "/lfs/uploads/") {
		if s.deleteAfter {
			if err := s.MemoryStore.Delete(ctx, key, version); err != nil {
				return err
			}
		}
		return errors.New("reservation deletion unavailable")
	}
	return s.MemoryStore.Delete(ctx, key, version)
}

func TestLFSQuotaInitializationCleanupFaultsPreserveExistingHistory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		quotaFailure      int
		ambiguousQuota    bool
		abortFailure      bool
		deleteFailure     bool
		deleteAfter       bool
		unexpectedPayload bool
		retained          bool
	}{
		{name: "dirty CAS", quotaFailure: 3, retained: true},
		{name: "ambiguous dirty CAS", quotaFailure: 3, ambiguousQuota: true, retained: true},
		{name: "abort", abortFailure: true, retained: true},
		{name: "delete", deleteFailure: true, retained: true},
		{name: "ambiguous delete", deleteFailure: true, deleteAfter: true},
		{name: "restore", quotaFailure: 4},
		{name: "ambiguous restore", quotaFailure: 4, ambiguousQuota: true},
		{name: "unexpected completed payload", unexpectedPayload: true, retained: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			seedQuotaHistory(t, store, metadata.ID, 3)
			previous := initializeQuota(t, store, metadata.ID)
			failure := errors.New("initialization result lost")
			fault := &quotaFaultStore{MemoryStore: objects, failWrite: 2, failWrites: map[int]bool{test.quotaFailure: true}, after: test.ambiguousQuota, failure: failure}
			store.objects = &quotaCleanupFaultStore{quotaFaultStore: fault, abortFailure: test.abortFailure,
				deleteFailure: test.deleteFailure, deleteAfter: test.deleteAfter, unexpectedPayload: test.unexpectedPayload}
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: previous.Bytes + 5}
			_, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), limits, allowLFS)
			if !errors.Is(err, failure) {
				t.Fatalf("fault injection = %v", err)
			}
			store.objects = objects
			quota := assertQuotaMatchesArtifacts(t, store, metadata.ID)
			wantBytes, wantObjects, wantReservations := previous.Bytes, previous.Objects, 0
			if test.retained {
				wantBytes += 5
				wantObjects++
				wantReservations = 1
			}
			if quota.Bytes != wantBytes || quota.Objects != wantObjects || len(quota.Reservations) != wantReservations {
				t.Fatalf("cleanup accounting=%+v, want bytes=%d objects=%d reservations=%d", quota, wantBytes, wantObjects, wantReservations)
			}
			_, err = store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("other")), 5, strings.NewReader("other"), limits, allowLFS)
			if test.retained && !errors.Is(err, ErrLFSQuota) || !test.retained && err != nil {
				t.Fatalf("cleanup retry admission=%v, retained=%v", err, test.retained)
			}
		})
	}
}

func TestLFSQuotaConcurrentDuplicatePayloadsRemainCharged(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 10}
	oid := lfsOID([]byte("hello"))
	first := &pausedLFSReader{reader: strings.NewReader("hello"), started: make(chan struct{}), release: make(chan struct{})}
	second := &pausedLFSReader{reader: strings.NewReader("hello"), started: make(chan struct{}), release: make(chan struct{})}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, first, limits, allowLFS)
		firstDone <- err
	}()
	select {
	case <-first.started:
	case err := <-firstDone:
		t.Fatalf("first upload did not start streaming: %v", err)
	}
	go func() {
		_, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, second, limits, allowLFS)
		secondDone <- err
	}()
	select {
	case <-second.started:
	case err := <-secondDone:
		close(first.release)
		<-firstDone
		t.Fatalf("second upload did not start streaming: %v", err)
	}
	_, thirdErr := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS)
	close(first.release)
	firstErr := <-firstDone
	// This completion collides with the verified record from the first upload.
	close(second.release)
	secondErr := <-secondDone
	if firstErr != nil || secondErr != nil || !errors.Is(thirdErr, ErrLFSQuota) {
		t.Fatalf("duplicate uploads: first=%v second=%v excess=%v", firstErr, secondErr, thirdErr)
	}
	quota := assertQuotaMatchesArtifacts(t, store, metadata.ID)
	if quota.Bytes != 10 || quota.Objects != 2 || len(quota.Reservations) != 0 {
		t.Fatalf("duplicate completed payload escaped quota: %+v", quota)
	}
	verified, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/verified/")
	if err != nil || len(verified) != 1 {
		t.Fatalf("duplicate verification records = %v, %v", verified, err)
	}
	body, _, err := store.LFSOpen(t.Context(), "alice", "demo", oid, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil || string(data) != "hello" {
		t.Fatalf("verified duplicate content = %q, %v", data, err)
	}
	report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || report.Deleted != 3 {
		t.Fatalf("duplicate orphan collection = %+v, %v", report, err)
	}
	quota = assertQuotaMatchesArtifacts(t, store, metadata.ID)
	if quota.Bytes != 0 || quota.Objects != 0 {
		t.Fatalf("GC retained duplicate payload charges: %+v", quota)
	}
}
