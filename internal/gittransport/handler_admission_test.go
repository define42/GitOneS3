package gittransport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type blockedRepositoryStore struct {
	storage.ObjectStore
	entered chan string
	resume  chan struct{}
}

func (s *blockedRepositoryStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if key == "repositories/alice/one.json" || key == "repositories/alice/two.json" || key == "repositories/alice/queued.json" {
		select {
		case s.entered <- key:
		case <-ctx.Done():
			return nil, storage.ObjectInfo{}, ctx.Err()
		}
		select {
		case <-s.resume:
		case <-ctx.Done():
			return nil, storage.ObjectInfo{}, ctx.Err()
		}
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestHandlerConcurrentHTTPAndSSHUseSharedBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		objects := storage.NewMemoryStore()
		seed, err := repository.New(objects)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"one", "two", "queued", "advertise"} {
			if _, err := seed.Create(t.Context(), "alice", repository.CreateInput{
				Name: name, CreatedBy: "alice", InitializeReadme: true,
				AuthorName: "Alice", AuthorEmail: "alice@example.test",
			}); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := seed.ReadGit(t.Context(), "alice", "one")
		if err != nil {
			t.Fatal(err)
		}
		blocked := &blockedRepositoryStore{ObjectStore: objects, entered: make(chan string, 4), resume: make(chan struct{})}
		store, err := repository.New(blocked)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := New(store, Options{MaxConcurrentOperations: 2, MaxQueuedOperations: 1, QueueTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		request := func(ctx context.Context, name string) *http.Request {
			body := pkt("want "+snapshot.References["refs/heads/main"]+"\n") + "0000" + pkt("done\n")
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/alice/"+name+".git/git-upload-pack", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			return r
		}
		httpDone := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request(t.Context(), "one"))
			httpDone <- w.Code
		}()
		if key := <-blocked.entered; key != "repositories/alice/one.json" {
			t.Fatalf("unexpected HTTP read: %s", key)
		}
		sshDone := make(chan error, 1)
		go func() {
			sshDone <- handler.ServeSSH(t.Context(), SSHRequest{Namespace: "alice", Repository: "two", Service: upload,
				Stream: &sshTestStream{input: bytes.NewReader([]byte("0000"))}})
		}()
		if key := <-blocked.entered; key != "repositories/alice/two.json" {
			t.Fatalf("unrelated SSH repository did not run concurrently: %s", key)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		queuedDone := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request(ctx, "queued"))
			queuedDone <- w.Code
		}()
		synctest.Wait()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request(t.Context(), "excess"))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("excess request not rejected as busy: %d", w.Code)
		}
		// HTTP advertisements have an independent metadata-only admission gate.
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/alice/advertise.git/info/refs?service=git-upload-pack", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("active transfers monopolized ref discovery: %d", w.Code)
		}
		cancel()
		if status := <-queuedDone; status != http.StatusServiceUnavailable {
			t.Fatalf("queued cancellation status = %d", status)
		}
		if len(blocked.entered) != 0 {
			t.Fatal("waiting request accessed storage before admission")
		}
		close(blocked.resume)
		if status := <-httpDone; status != http.StatusOK {
			t.Fatalf("admitted HTTP fetch failed: %d", status)
		}
		if err := <-sshDone; err != nil {
			t.Fatalf("admitted SSH request failed: %v", err)
		}
		if len(handler.operations.active) != 0 || len(handler.operations.waiting) != 0 {
			t.Fatal("completed operations leaked admission capacity")
		}
	})
}
