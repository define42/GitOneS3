package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestShutdownWaitsForLFSCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		objects := &lfsShutdownStore{MemoryStore: storage.NewMemoryStore()}
		repositories, err := repository.New(objects)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := repositories.Create(t.Context(), "alice", repository.CreateInput{
			Name: "demo", DefaultBranch: "main", CreatedBy: "alice",
		})
		if err != nil {
			t.Fatal(err)
		}
		const payload = "hello"
		digest := sha256.Sum256([]byte(payload))
		oid := hex.EncodeToString(digest[:])
		limits := repository.LFSLimits{MaxObjectBytes: int64(len(payload)), MaxRepositoryBytes: int64(len(payload))}
		finalizing := make(chan struct{})
		canceled := make(chan struct{})
		uploadDone := make(chan error, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// LFS transfers have a longer body deadline. Avoid letting the
			// default body deadline cancel this request at the shutdown boundary.
			if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute)); err != nil {
				uploadDone <- err
				return
			}
			authorizations := 0
			authorize := func(ctx context.Context) error {
				authorizations++
				if authorizations == 1 {
					return nil
				}
				objects.delayCleanup.Store(true)
				close(finalizing)
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			}
			_, err := repositories.LFSUpload(r.Context(), "alice", "demo", oid, int64(len(payload)), r.Body, limits, authorize)
			uploadDone <- err
		})
		server, err := New("unused", handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		client, connection := net.Pipe()
		listener := &pipeListener{connection: connection, closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		defer func() {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
		}()
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.serve(ctx, listener) }()
		writeRequest(t, client, "PUT /lfs HTTP/1.1\r\nHost: localhost\r\nContent-Length: 5\r\n\r\n"+payload)
		select {
		case <-finalizing:
		case err := <-uploadDone:
			t.Fatalf("upload did not reach locked finalization: %v", err)
		case <-time.After(time.Second):
			t.Fatal("upload did not reach locked finalization")
		}
		if _, err := repositories.GetMaintenanceLock(t.Context(), "alice", "demo"); err != nil {
			t.Fatalf("finalizing upload has no lock: %v", err)
		}

		started := time.Now()
		cancel()
		time.Sleep(defaultShutdownTimeout - time.Second)
		synctest.Wait()
		select {
		case <-canceled:
			t.Fatal("shutdown canceled an upload before its grace period ended")
		case err := <-serveDone:
			t.Fatalf("shutdown returned before its grace period ended: %v", err)
		default:
		}
		if err := <-serveDone; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown = %v, want the graceful shutdown deadline", err)
		}
		if elapsed := time.Since(started); elapsed < defaultShutdownTimeout+2*lfsShutdownDeleteDelay {
			t.Errorf("shutdown returned after %s before delayed LFS cleanup could finish", elapsed)
		}
		var uploadErr error
		select {
		case uploadErr = <-uploadDone:
		default:
			t.Error("shutdown returned while LFS cleanup was still running")
			// Join even on regression so synctest can report the assertion
			// without a second failure from an abandoned cleanup goroutine.
			uploadErr = <-uploadDone
		}
		if !errors.Is(uploadErr, context.Canceled) {
			t.Fatalf("interrupted upload = %v, want cancellation", uploadErr)
		}
		if objects.delayedDeletes.Load() != 2 || objects.aborts.Load() != 1 {
			t.Fatalf("cleanup: delayed lock deletes = %d, multipart aborts = %d", objects.delayedDeletes.Load(), objects.aborts.Load())
		}
		if _, err := repositories.GetMaintenanceLock(t.Context(), "alice", "demo"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("shutdown left a durable repository lock: %v", err)
		}
		reservations, err := objects.List(t.Context(), "repos/"+metadata.ID+"/lfs/uploads/")
		if err != nil || len(reservations) != 0 {
			t.Fatalf("shutdown left upload reservations: %v, %v", reservations, err)
		}
		if _, err := repositories.LFSStat(t.Context(), "alice", "demo", oid); !errors.Is(err, repository.ErrLFSMissing) {
			t.Fatalf("interrupted upload was published: %v", err)
		}
		objects.delayCleanup.Store(false)
		if _, err := repositories.LFSUpload(t.Context(), "alice", "demo", oid, int64(len(payload)), strings.NewReader(payload), limits, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("shutdown did not release upload quota for a retry: %v", err)
		}
	})
}

const lfsShutdownDeleteDelay = 2 * time.Second

type lfsShutdownStore struct {
	*storage.MemoryStore
	delayCleanup   atomic.Bool
	delayedDeletes atomic.Int64
	aborts         atomic.Int64
}

func (s *lfsShutdownStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if s.delayCleanup.Load() && strings.HasSuffix(key, "/maintenance-lock") {
		s.delayedDeletes.Add(1)
		timer := time.NewTimer(lfsShutdownDeleteDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.MemoryStore.Delete(ctx, key, version)
}

func (s *lfsShutdownStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	s.aborts.Add(1)
	return s.MemoryStore.AbortMultipart(ctx, key, uploadID)
}
