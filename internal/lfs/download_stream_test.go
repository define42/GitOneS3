package lfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestDownloadStreamsBeforeEOFAndClosesOnCancellation(t *testing.T) {
	// A streaming download must work even when temporary files cannot be created.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "unavailable"))
	upstream := &downloadStreamStore{MemoryStore: storage.NewMemoryStore()}
	store, err := repository.New(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "demo", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), 256<<10)
	oid := objectID(data)
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", oid, int64(len(data)), bytes.NewReader(data),
		repository.LFSLimits{MaxObjectBytes: 1 << 20, MaxRepositoryBytes: 1 << 20}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	h, err := New(store, Options{PublicURL: "https://gitone.example"})
	if err != nil {
		t.Fatal(err)
	}
	upstream.enabled = true
	upstream.closed = make(chan struct{})
	upstream.canceled = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &streamObserver{headers: make(http.Header), written: make(chan int, 1)}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/alice/demo.git/info/lfs/objects/"+oid, nil)
	result := make(chan any, 1)
	go func() {
		defer func() { result <- recover() }()
		h.ServeHTTP(w, request)
	}()
	select {
	case size := <-w.written:
		if size <= 0 || size > 64<<10 {
			t.Fatalf("first write was not bounded: %d", size)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("handler waited for the whole object before writing its first bytes")
	}
	// The source remains incomplete and blocks its second Read until canceled.
	cancel()
	select {
	case panicValue := <-result:
		panicErr, ok := panicValue.(error)
		if !ok || !errors.Is(panicErr, http.ErrAbortHandler) {
			t.Fatalf("failed stream must abort HTTP response: %v", panicValue)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not stop after request cancellation")
	}
	select {
	case <-upstream.canceled:
	default:
		t.Fatal("S3 body did not observe request cancellation")
	}
	select {
	case <-upstream.closed:
	default:
		t.Fatal("S3 body was not closed")
	}
	if w.status != http.StatusOK || w.headers.Get("Content-Length") != "262144" {
		t.Fatalf("stream metadata: %d, %v", w.status, w.headers)
	}
}

type downloadStreamStore struct {
	*storage.MemoryStore
	enabled  bool
	closed   chan struct{}
	canceled chan struct{}
}

func (s *downloadStreamStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if !s.enabled || !strings.Contains(key, "/lfs/objects/") {
		return s.MemoryStore.Get(ctx, key)
	}
	info, err := s.Head(ctx, key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	return &pausedDownload{ctx: ctx, closed: s.closed, canceled: s.canceled}, info, nil
}

type pausedDownload struct {
	ctx      context.Context
	sent     bool
	closed   chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (r *pausedDownload) Read(p []byte) (int, error) {
	if len(p) > 64<<10 {
		return 0, errors.New("download requested an oversized buffer")
	}
	if !r.sent {
		r.sent = true
		for i := range p {
			p[i] = 'x'
		}
		return len(p), nil
	}
	<-r.ctx.Done()
	close(r.canceled)
	return 0, r.ctx.Err()
}
func (r *pausedDownload) Close() error { r.once.Do(func() { close(r.closed) }); return nil }

type streamObserver struct {
	headers http.Header
	status  int
	written chan int
}

func (w *streamObserver) Header() http.Header    { return w.headers }
func (w *streamObserver) WriteHeader(status int) { w.status = status }
func (w *streamObserver) Write(p []byte) (int, error) {
	select {
	case w.written <- len(p):
	default:
	}
	return len(p), nil
}
