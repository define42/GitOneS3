package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func lfsOID(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func allowLFS(context.Context) error { return nil }

func lfsLimits() LFSLimits { return LFSLimits{MaxObjectBytes: 32 << 20, MaxRepositoryBytes: 64 << 20} }

func lfsFixture(t *testing.T) (*Store, *storage.MemoryStore, Metadata) {
	t.Helper()
	store, objects := newTestStore(t)
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
	if err != nil {
		t.Fatal(err)
	}
	return store, objects, metadata
}

func TestLFSUploadAndStreamDownload(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	data := bytes.Repeat([]byte("streaming LFS content!"), 500_000)
	oid := lfsOID(data)
	input := &boundedLFSReader{reader: bytes.NewReader(data)}
	got, err := store.LFSUpload(t.Context(), "alice", "demo", oid, int64(len(data)), input, lfsLimits(), allowLFS)
	if err != nil || got.OID != oid || got.Size != int64(len(data)) || input.maxRead > LFSPartBytes {
		t.Fatalf("upload = %+v, %v, maximum read %d", got, err, input.maxRead)
	}
	stat, err := store.LFSStat(t.Context(), "alice", "demo", oid)
	if err != nil || stat != got {
		t.Fatalf("stat = %+v, %v", stat, err)
	}
	body, info, err := store.LFSOpen(t.Context(), "alice", "demo", oid, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	n, copyErr := io.Copy(digest, body)
	if err := errors.Join(copyErr, body.Close()); err != nil || n != int64(len(data)) || info != got || hex.EncodeToString(digest.Sum(nil)) != oid {
		t.Fatalf("download = %d, %+v, %v", n, info, err)
	}
	body, _, err = store.LFSOpen(t.Context(), "alice", "demo", oid, 19, 27)
	if err != nil {
		t.Fatal(err)
	}
	partial, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil || !bytes.Equal(partial, data[19:46]) {
		t.Fatalf("range = %q, %v", partial, err)
	}
	before, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/")
	if err != nil || len(before) != 2 {
		t.Fatalf("stored LFS objects = %d, %v", len(before), err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, int64(len(data)), bytes.NewReader(data), lfsLimits(), allowLFS); err != nil {
		t.Fatalf("retry = %v", err)
	}
	after, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/")
	if err != nil || len(after) != len(before) {
		t.Fatalf("retry created more objects: %d, %v", len(after), err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, int64(len(data)), strings.NewReader("wrong"), lfsLimits(), allowLFS); !errors.Is(err, ErrLFSHashMismatch) {
		t.Fatalf("bad duplicate upload = %v", err)
	}
	if _, err := store.LFSStat(t.Context(), "bob", "demo", oid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-repository access = %v", err)
	}
}

type boundedLFSReader struct {
	reader  io.Reader
	maxRead int
}

func (r *boundedLFSReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	return r.reader.Read(p)
}

func TestLFSUploadRejectsInvalidContentAndReleasesQuota(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data string
		size int64
	}{
		{name: "wrong hash", data: "other", size: 5},
		{name: "short body", data: "body", size: 5},
		{name: "long body", data: "body!!", size: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			oid := lfsOID([]byte("hello"))
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, test.size, strings.NewReader(test.data), limits, allowLFS); !errors.Is(err, ErrLFSHashMismatch) {
				t.Fatalf("invalid upload = %v", err)
			}
			if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); !errors.Is(err, ErrLFSMissing) {
				t.Fatalf("invalid upload published = %v", err)
			}
			artifacts, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/")
			if err != nil || len(artifacts) != 0 {
				t.Fatalf("failed upload retained quota/artifacts = %v, %v", artifacts, err)
			}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
				t.Fatalf("retry after failure = %v", err)
			}
		})
	}
}

func TestLFSUploadEmptyAndLimits(t *testing.T) {
	t.Parallel()
	store, _, _ := lfsFixture(t)
	empty := lfsOID(nil)
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", empty, 0, strings.NewReader(""), lfsLimits(), allowLFS); err != nil {
		t.Fatalf("empty upload = %v", err)
	}
	body, object, err := store.LFSOpen(t.Context(), "alice", "demo", empty, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil || len(data) != 0 || object.Size != 0 {
		t.Fatalf("empty download = %v, %v", data, err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", empty, 6, strings.NewReader(""), LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}, allowLFS); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversized upload = %v", err)
	}
	if _, _, err := store.LFSOpen(t.Context(), "alice", "demo", empty, 0, 1); !errors.Is(err, storage.ErrInvalidRange) {
		t.Fatalf("empty range = %v", err)
	}
}

func TestLFSUploadFinalAuthorizationAndOrphanCleanup(t *testing.T) {
	t.Parallel()
	for _, callsAllowed := range []int{1, 2} {
		t.Run(fmt.Sprintf("authorize %d calls", callsAllowed), func(t *testing.T) {
			t.Parallel()
			store, objects, metadata := lfsFixture(t)
			calls := 0
			authorize := func(context.Context) error {
				calls++
				if calls > callsAllowed {
					return errors.New("permission revoked")
				}
				return nil
			}
			oid := lfsOID([]byte("hello"))
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), lfsLimits(), authorize); !errors.Is(err, ErrForbidden) {
				t.Fatalf("revoked upload = %v", err)
			}
			if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); !errors.Is(err, ErrLFSMissing) {
				t.Fatalf("revoked content published: %v", err)
			}
			report, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
			if err != nil || report.Deleted != callsAllowed-1 {
				t.Fatalf("orphan cleanup = %+v, %v", report, err)
			}
			artifacts, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/")
			if err != nil || len(artifacts) != 0 {
				t.Fatalf("cleanup retained artifacts = %v, %v", artifacts, err)
			}
		})
	}
}

type pausedLFSReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *pausedLFSReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return r.reader.Read(p)
}

func TestLFSQuotaIncludesConcurrentReservations(t *testing.T) {
	t.Parallel()
	store, _, _ := lfsFixture(t)
	limits := LFSLimits{MaxObjectBytes: 10, MaxRepositoryBytes: 10}
	paused := &pausedLFSReader{reader: strings.NewReader("12345678"), started: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("12345678")), 8, paused, limits, allowLFS)
		finished <- err
	}()
	<-paused.started
	_, quotaErr := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("abc")), 3, strings.NewReader("abc"), limits, allowLFS)
	_, duplicateErr := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("12345678")), 8, strings.NewReader("12345678"), limits, allowLFS)
	report, gcErr := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	close(paused.release)
	uploadErr := <-finished
	if !errors.Is(quotaErr, ErrLFSQuota) || !errors.Is(duplicateErr, ErrConflict) || gcErr != nil || report.Deleted != 0 || uploadErr != nil {
		t.Fatalf("quota=%v duplicate=%v gc=%+v/%v first=%v", quotaErr, duplicateErr, report, gcErr, uploadErr)
	}
}

func TestLFSDuplicateUploadDetectsConcurrentCollection(t *testing.T) {
	t.Parallel()
	store, _, _ := lfsFixture(t)
	oid := lfsOID([]byte("hello"))
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
		t.Fatal(err)
	}
	paused := &pausedLFSReader{reader: strings.NewReader("hello"), started: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, paused, lfsLimits(), allowLFS)
		finished <- err
	}()
	<-paused.started
	report, gcErr := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	close(paused.release)
	uploadErr := <-finished
	if gcErr != nil || report.Deleted != 2 || !errors.Is(uploadErr, ErrConflict) || !errors.Is(uploadErr, ErrLFSMissing) {
		t.Fatalf("collection=%+v/%v duplicate upload=%v", report, gcErr, uploadErr)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), lfsLimits(), allowLFS); err != nil {
		t.Fatalf("retry after collection = %v", err)
	}
}

func TestLFSCancelledUploadReleasesReservation(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelLFSReader{cancel: cancel}
	_, err := store.LFSUpload(ctx, "alice", "demo", lfsOID([]byte("hello")), 5, reader, lfsLimits(), allowLFS)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload = %v", err)
	}
	artifacts, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/")
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("canceled upload retained reservation = %v, %v", artifacts, err)
	}
}

type cancelLFSReader struct{ cancel context.CancelFunc }

func (r *cancelLFSReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, context.Canceled
}

type lfsFailureStore struct {
	*storage.MemoryStore
	failReservationUpdate bool
	failVerified          bool
	failAbort             bool
}

func (s *lfsFailureStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if s.failAbort {
		return errors.New("provider failed to abort multipart upload")
	}
	return s.MemoryStore.AbortMultipart(ctx, key, uploadID)
}

func (s *lfsFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.ObjectInfo, error) {
	if s.failVerified && strings.Contains(key, "/lfs/verified/") {
		return storage.ObjectInfo{}, errors.New("verified record publication failed")
	}
	info, err := s.MemoryStore.Put(ctx, key, body, size, options)
	if err == nil && s.failReservationUpdate && strings.Contains(key, "/lfs/uploads/") && options.IfMatch != "" {
		return storage.ObjectInfo{}, errors.New("ambiguous reservation update")
	}
	return info, err
}

func TestLFSUploadStorageFailuresReleaseQuotaAndCollectOrphans(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"reservation update", "verified publication"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			objects := &lfsFailureStore{MemoryStore: storage.NewMemoryStore(), failReservationUpdate: name == "reservation update", failVerified: name == "verified publication"}
			store, err := New(objects)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
			if err != nil {
				t.Fatal(err)
			}
			oid := lfsOID([]byte("hello"))
			limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err == nil {
				t.Fatal("storage failure was ignored")
			}
			reservations, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/uploads/")
			if err != nil || len(reservations) != 0 {
				t.Fatalf("failed storage retained quota: %v, %v", reservations, err)
			}
			if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); !errors.Is(err, ErrLFSMissing) {
				t.Fatalf("failed upload published: %v", err)
			}
			gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
			wantDeleted := 0
			if objects.failVerified {
				wantDeleted = 1
			}
			if err != nil || gc.Deleted != wantDeleted {
				t.Fatalf("storage orphan GC = %+v, %v", gc, err)
			}
			objects.failReservationUpdate, objects.failVerified = false, false
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("hello"), limits, allowLFS); err != nil {
				t.Fatalf("retry retained quota: %v", err)
			}
		})
	}
}

func TestLFSQuotaCountsUnpublishedCompletedContentUntilGC(t *testing.T) {
	t.Parallel()
	store, _, _ := lfsFixture(t)
	limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
	calls := 0
	authorize := func(context.Context) error {
		calls++
		if calls == 3 {
			return errors.New("revoked after multipart completion")
		}
		return nil
	}
	_, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("hello"), limits, authorize)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("upload after permission revocation = %v", err)
	}
	oid := lfsOID([]byte("other"))
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("other"), limits, allowLFS); !errors.Is(err, ErrLFSQuota) {
		t.Fatalf("unpublished completed bytes bypassed quota: %v", err)
	}
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != 1 {
		t.Fatalf("collect completed orphan = %+v, %v", gc, err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("other"), limits, allowLFS); err != nil {
		t.Fatalf("GC did not release physical quota: %v", err)
	}
}

func TestLFSFailedAbortKeepsQuotaReservationUntilGC(t *testing.T) {
	t.Parallel()
	objects := &lfsFailureStore{MemoryStore: storage.NewMemoryStore(), failAbort: true}
	store, err := New(objects)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Create(t.Context(), "alice", createInput("demo", false))
	if err != nil {
		t.Fatal(err)
	}
	limits := LFSLimits{MaxObjectBytes: 5, MaxRepositoryBytes: 5}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte("hello")), 5, strings.NewReader("wrong"), limits, allowLFS); !errors.Is(err, ErrLFSHashMismatch) {
		t.Fatalf("mismatched upload = %v", err)
	}
	oid := lfsOID([]byte("other"))
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("other"), limits, allowLFS); !errors.Is(err, ErrLFSQuota) {
		t.Fatalf("failed abort released quota: %v", err)
	}
	reservations, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/uploads/")
	if err != nil || len(reservations) != 1 {
		t.Fatalf("missing recovery reservation: %v, %v", reservations, err)
	}
	reservation, err := store.readLFSReservation(t.Context(), metadata.ID, reservations[0])
	if err != nil {
		t.Fatal(err)
	}
	reservation.CreatedAt = time.Now().Add(-2 * LFSReservationLifetime)
	reservation.ExpiresAt = reservation.CreatedAt.Add(LFSReservationLifetime)
	if _, err := store.putLFSJSON(t.Context(), reservations[0].Key, reservation, storage.PutOptions{IfMatch: reservations[0].Version}); err != nil {
		t.Fatal(err)
	}
	objects.failAbort = false
	gc, err := store.GarbageCollect(t.Context(), "alice", "demo", GCOptions{Apply: true, GracePeriod: time.Nanosecond})
	if err != nil || gc.Deleted != 1 {
		t.Fatalf("aborted reservation recovery = %+v, %v", gc, err)
	}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, 5, strings.NewReader("other"), limits, allowLFS); err != nil {
		t.Fatalf("abort recovery retained quota: %v", err)
	}
}
