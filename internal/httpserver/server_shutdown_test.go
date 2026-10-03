package httpserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestServerShutdownDrainsRequestsBeforeCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		client, cancel, done, _ := runShutdownServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			time.Sleep(10 * time.Second)
			if err := r.Context().Err(); err != nil {
				t.Errorf("request canceled during graceful drain: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		writeRequest(t, client, "GET /finish HTTP/1.1\r\nHost: localhost\r\n\r\n")
		<-entered
		started := time.Now()
		cancel()
		readNoContent(t, bufio.NewReader(client))
		if err := <-done; err != nil {
			t.Fatalf("graceful shutdown: %v", err)
		}
		if elapsed := time.Since(started); elapsed < 10*time.Second || elapsed >= defaultShutdownTimeout {
			t.Fatalf("graceful shutdown took %v", elapsed)
		}
	})
}

func TestServerShutdownWaitsForPublicationLockRelease(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		name := "context cancellation"
		if failure {
			name = "listener failure"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				objects := &shutdownLockStore{MemoryStore: storage.NewMemoryStore()}
				store, err := repository.New(objects)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Create(t.Context(), "alice", repository.CreateInput{
					Name: "demo", InitializeReadme: true, CreatedBy: "user:alice",
					AuthorName: "Alice", AuthorEmail: "alice@example.com",
				}); err != nil {
					t.Fatal(err)
				}
				base, err := store.ReadGit(t.Context(), "alice", "demo")
				if err != nil {
					t.Fatal(err)
				}
				updates := []repository.RefUpdate{{Name: "refs/tags/review", New: base.References["refs/heads/main"]}}
				entered := make(chan struct{})
				publicationDone := make(chan error, 1)
				client, cancel, done, listener := runShutdownServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					ctx, stop := context.WithTimeout(r.Context(), 90*time.Second)
					defer stop()
					publicationDone <- store.PublishPack(ctx, base, updates, nil, func(ctx context.Context) error {
						close(entered)
						<-ctx.Done()
						if !errors.Is(ctx.Err(), context.Canceled) {
							t.Errorf("publication context ended before server cancellation: %v", ctx.Err())
						}
						return ctx.Err()
					})
				}))
				writeRequest(t, client, "GET /publish HTTP/1.1\r\nHost: localhost\r\n\r\n")
				<-entered
				started := time.Now()
				if failure {
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				time.Sleep(defaultShutdownTimeout + time.Second)
				if _, err := store.GetMaintenanceLock(t.Context(), "alice", "demo"); err != nil {
					t.Fatalf("expected lock during delayed cleanup: %v", err)
				}
				select {
				case err := <-done:
					t.Fatalf("server returned before releasing the publication lock: %v", err)
				default:
				}
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("forced drain error = %v, want deadline exceeded", err)
				}
				if elapsed := time.Since(started); elapsed != defaultShutdownTimeout+2*time.Second {
					t.Fatalf("shutdown waited %v, want grace plus lock cleanup", elapsed)
				}
				if err := <-publicationDone; !errors.Is(err, repository.ErrForbidden) {
					t.Fatalf("publication error = %v, want canceled authorization", err)
				}
				if _, err := store.GetMaintenanceLock(t.Context(), "alice", "demo"); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("publication lock survived shutdown: %v", err)
				}
				if err := store.PublishPack(t.Context(), base, updates, nil, func(context.Context) error { return nil }); err != nil {
					t.Fatalf("publication after restart remained blocked: %v", err)
				}
			})
		})
	}
}

func TestServerShutdownClosesStalledRequestBody(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		readDone := make(chan error, 1)
		client, cancel, done, _ := runShutdownServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
				t.Error(err)
				return
			}
			close(entered)
			_, err := io.Copy(io.Discard, r.Body)
			// Model cancellation-independent cleanup after a blocked body read.
			time.Sleep(time.Second)
			readDone <- err
		}))
		writeRequest(t, client, "POST /upload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n")
		<-entered
		started := time.Now()
		cancel()
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forced drain error = %v", err)
		}
		if err := <-readDone; err == nil {
			t.Fatal("stalled body read unexpectedly succeeded")
		}
		if elapsed := time.Since(started); elapsed != defaultShutdownTimeout+time.Second {
			t.Fatalf("stalled body cleanup took %v", elapsed)
		}
	})
}

func TestServerShutdownBoundsUnresponsiveHandler(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		client, cancel, done, _ := runShutdownServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			defer close(finished)
			close(entered)
			<-release // Deliberately ignore cancellation to exercise the final bound.
		}))
		defer func() { close(release); <-finished }()
		writeRequest(t, client, "GET /stuck HTTP/1.1\r\nHost: localhost\r\n\r\n")
		<-entered
		started := time.Now()
		cancel()
		err := <-done
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1 HTTP request handlers") {
			t.Fatalf("unresponsive handler error = %v", err)
		}
		if elapsed := time.Since(started); elapsed != defaultShutdownTimeout+defaultCleanupTimeout {
			t.Fatalf("unresponsive handler held shutdown for %v", elapsed)
		}
	})
}

type shutdownLockStore struct{ *storage.MemoryStore }

func (s *shutdownLockStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if strings.HasSuffix(key, "/maintenance-lock") {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.MemoryStore.Delete(ctx, key, version)
}

func runShutdownServer(t *testing.T, handler http.Handler) (net.Conn, context.CancelFunc, <-chan error, *pipeListener) {
	t.Helper()
	server, err := New("unused", handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	client, connection := net.Pipe()
	listener := &pipeListener{connection: connection, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		results <- server.serve(ctx, listener)
	}()
	t.Cleanup(func() {
		cancel()
		if err := client.Close(); err != nil {
			t.Error(err)
		}
		<-finished
	})
	return client, cancel, results, listener
}
