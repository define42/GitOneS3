package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/metrics"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestNewStore(t *testing.T) {
	t.Parallel()
	if _, err := metrics.NewStore(nil); err == nil {
		t.Fatal("NewStore(nil) succeeded")
	}
}

func TestStoreOperations(t *testing.T) {
	t.Parallel()
	store := newStore(t, storage.NewMemoryStore())
	const key = "private-repo/secret-object"
	info, err := store.Put(t.Context(), key, strings.NewReader("abcdef"), 6, storage.PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != key || info.Size != 6 {
		t.Fatalf("Put metadata = %+v", info)
	}
	body, gotInfo, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo != info {
		t.Fatalf("Get metadata = %+v, want %+v", gotInfo, info)
	}
	if _, err := io.ReadFull(body, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	assertSample(t, store, `gitone_storage_active_operations{operation="get"}`, 1)
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="get"}`, 2)
	assertSample(t, store, `gitone_storage_operations_total{operation="get",result="success"}`, 0)
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	// Repeated closes must not produce additional completed operations.
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	body, gotInfo, err = store.GetRange(t.Context(), key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if string(data) != "cde" || gotInfo != info {
		t.Fatalf("GetRange = %q, %+v", data, gotInfo)
	}
	if got, err := store.Head(t.Context(), key); err != nil || got != info {
		t.Fatalf("Head = %+v, %v", got, err)
	}
	if got, err := store.List(t.Context(), "private-repo/"); err != nil || len(got) != 1 || got[0] != info {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if got, err := store.ListPage(t.Context(), "private-repo/", "", 1); err != nil || len(got.Objects) != 1 || got.Objects[0] != info {
		t.Fatalf("ListPage = %+v, %v", got, err)
	}
	if err := store.Delete(t.Context(), key, info.Version); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"put", "get", "get_range", "head", "delete", "list", "list_page"} {
		assertSample(t, store, fmt.Sprintf(`gitone_storage_operations_total{operation=%q,result="success"}`, op), 1)
		assertSample(t, store, fmt.Sprintf(`gitone_storage_active_operations{operation=%q}`, op), 0)
		assertSample(t, store, fmt.Sprintf(`gitone_storage_operation_duration_seconds_count{operation=%q}`, op), 1)
		assertSample(t, store, fmt.Sprintf(`gitone_storage_operation_duration_seconds_bucket{operation=%q,le="+Inf"}`, op), 1)
	}
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="put"}`, 6)
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="get"}`, 2)
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="get_range"}`, 3)
	if output := scrape(t.Context(), store).Body.String(); strings.Contains(output, key) || strings.Contains(output, "private-repo") {
		t.Fatal("metrics include a private object key")
	}
}

func TestStorePutPreservesSeekAndCountsRetries(t *testing.T) {
	t.Parallel()
	inner := &stubStore{ObjectStore: storage.NewMemoryStore()}
	inner.put = func(_ context.Context, _ string, body io.Reader, _ int64, _ storage.PutOptions) (storage.ObjectInfo, error) {
		seekable, ok := body.(io.ReadSeeker)
		if !ok {
			t.Fatal("Put body lost seek support")
		}
		if _, err := io.Copy(io.Discard, seekable); err != nil {
			t.Fatal(err)
		}
		if _, err := seekable.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, seekable); err != nil {
			t.Fatal(err)
		}
		return storage.ObjectInfo{}, nil
	}
	store := newStore(t, inner)
	if _, err := store.Put(t.Context(), "object", strings.NewReader("retry"), 5, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="put"}`, 10)
	assertSample(t, store, `gitone_storage_operations_total{operation="put",result="success"}`, 1)
}

func TestStorePutNilBody(t *testing.T) {
	t.Parallel()
	store := newStore(t, storage.NewMemoryStore())
	if _, err := store.Put(t.Context(), "object", nil, 0, storage.PutOptions{}); err == nil {
		t.Fatal("nil body accepted")
	}
	assertSample(t, store, `gitone_storage_operations_total{operation="put",result="error"}`, 1)
	assertSample(t, store, `gitone_storage_active_operations{operation="put"}`, 0)
}

func TestStoreReadErrors(t *testing.T) {
	t.Parallel()
	readFailure := errors.New("read failed")
	closeFailure := errors.New("close failed")
	for _, test := range []struct {
		name     string
		readErr  error
		closeErr error
		result   string
	}{
		{name: "EOF", readErr: io.EOF, result: "success"},
		{name: "read failure", readErr: readFailure, result: "error"},
		{name: "close after EOF", readErr: io.EOF, closeErr: closeFailure, result: "error"},
		{name: "canceled read", readErr: context.Canceled, result: "canceled"},
		{name: "timed out close", readErr: io.EOF, closeErr: context.DeadlineExceeded, result: "deadline_exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, ranged := range []bool{false, true} {
				inner := &stubStore{ObjectStore: storage.NewMemoryStore()}
				body := &errorBody{readErr: test.readErr, closeErr: test.closeErr}
				inner.get = func(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
					return body, storage.ObjectInfo{Size: 12}, nil
				}
				store := newStore(t, inner)
				var reader io.ReadCloser
				var err error
				op := "get"
				if ranged {
					op = "get_range"
					reader, _, err = store.GetRange(t.Context(), "object", 0, 12)
				} else {
					reader, _, err = store.Get(t.Context(), "object")
				}
				if err != nil {
					t.Fatal(err)
				}
				n, err := reader.Read(make([]byte, 12))
				if n != 3 || !errors.Is(err, test.readErr) {
					t.Fatalf("Read = %d, %v; want 3, %v", n, err, test.readErr)
				}
				assertSample(t, store, fmt.Sprintf(`gitone_storage_active_operations{operation=%q}`, op), 1)
				for range 2 {
					if err := reader.Close(); !errors.Is(err, test.closeErr) {
						t.Fatalf("Close = %v, want %v", err, test.closeErr)
					}
				}
				if body.closes != 1 {
					t.Fatalf("underlying body closed %d times", body.closes)
				}
				assertSample(t, store, fmt.Sprintf(`gitone_storage_active_operations{operation=%q}`, op), 0)
				assertSample(t, store, fmt.Sprintf(`gitone_storage_operations_total{operation=%q,result=%q}`, op, test.result), 1)
				assertSample(t, store, fmt.Sprintf(`gitone_storage_transferred_bytes_total{operation=%q}`, op), 3)
			}
		})
	}
}

func TestStoreGetFailure(t *testing.T) {
	t.Parallel()
	store := newStore(t, storage.NewMemoryStore())
	if _, _, err := store.Get(t.Context(), "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if _, _, err := store.GetRange(t.Context(), "missing", 0, 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetRange error = %v", err)
	}
	for _, op := range []string{"get", "get_range"} {
		assertSample(t, store, fmt.Sprintf(`gitone_storage_active_operations{operation=%q}`, op), 0)
		assertSample(t, store, fmt.Sprintf(`gitone_storage_operations_total{operation=%q,result="not_found"}`, op), 1)
	}
}

func TestStoreResults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "not_found", err: storage.ErrNotFound},
		{name: "already_exists", err: storage.ErrAlreadyExists},
		{name: "precondition_failed", err: storage.ErrPreconditionFailed},
		{name: "conflict", err: storage.ErrConditionalConflict},
		{name: "canceled", err: context.Canceled},
		{name: "deadline_exceeded", err: context.DeadlineExceeded},
		{name: "invalid_range", err: storage.ErrInvalidRange},
		{name: "error", err: errors.New("provider failed for a private object")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inner := &stubStore{ObjectStore: storage.NewMemoryStore()}
			inner.head = func(context.Context, string) (storage.ObjectInfo, error) {
				return storage.ObjectInfo{}, fmt.Errorf("wrapped: %w", test.err)
			}
			store := newStore(t, inner)
			if _, err := store.Head(t.Context(), "object"); !errors.Is(err, test.err) {
				t.Fatalf("Head error = %v, want %v", err, test.err)
			}
			assertSample(t, store, fmt.Sprintf(`gitone_storage_operations_total{operation="head",result=%q}`, test.name), 1)
			assertSample(t, store, `gitone_storage_active_operations{operation="head"}`, 0)
		})
	}
}

func TestStoreDurationHistogram(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &stubStore{ObjectStore: storage.NewMemoryStore()}
		inner.head = func(context.Context, string) (storage.ObjectInfo, error) {
			time.Sleep(20 * time.Millisecond)
			return storage.ObjectInfo{}, nil
		}
		store := newStore(t, inner)
		if _, err := store.Head(t.Context(), "object"); err != nil {
			t.Fatal(err)
		}
		assertSample(t, store, `gitone_storage_operation_duration_seconds_bucket{operation="head",le="0.01"}`, 0)
		assertSample(t, store, `gitone_storage_operation_duration_seconds_bucket{operation="head",le="0.025"}`, 1)
		assertSample(t, store, `gitone_storage_operation_duration_seconds_bucket{operation="head",le="60"}`, 1)
		assertSample(t, store, `gitone_storage_operation_duration_seconds_bucket{operation="head",le="+Inf"}`, 1)
		assertSample(t, store, `gitone_storage_operation_duration_seconds_sum{operation="head"}`, 0.02)
	})
}

func TestStoreServeHTTP(t *testing.T) {
	t.Parallel()
	store := newStore(t, storage.NewMemoryStore())
	response := scrape(t.Context(), store)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET response = %d, %v", response.Code, response.Header())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("metrics must not be cached")
	}
	response = httptest.NewRecorder()
	store.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/metrics", nil))
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("HEAD response = %d, %q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	store.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/metrics", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST response = %d, %v", response.Code, response.Header())
	}
}

func TestStoreConcurrentOperationsAndScrapes(t *testing.T) {
	t.Parallel()
	inner := storage.NewMemoryStore()
	if _, err := inner.Put(t.Context(), "object", strings.NewReader("payload"), 7, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	store := newStore(t, inner)
	var workers sync.WaitGroup
	for range 40 {
		workers.Go(func() {
			body, _, err := store.Get(t.Context(), "object")
			if err != nil {
				t.Error(err)
				return
			}
			_, readErr := io.Copy(io.Discard, body)
			if err := errors.Join(readErr, body.Close()); err != nil {
				t.Error(err)
			}
			if response := scrape(t.Context(), store); response.Code != http.StatusOK {
				t.Errorf("scrape status = %d", response.Code)
			}
		})
	}
	workers.Wait()
	assertSample(t, store, `gitone_storage_operations_total{operation="get",result="success"}`, 40)
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="get"}`, 280)
	assertSample(t, store, `gitone_storage_active_operations{operation="get"}`, 0)
}

type stubStore struct {
	storage.ObjectStore
	put  func(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.ObjectInfo, error)
	get  func(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error)
	head func(context.Context, string) (storage.ObjectInfo, error)
}

func (s *stubStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if s.put != nil {
		return s.put(ctx, key, body, size, opts)
	}
	return s.ObjectStore.Put(ctx, key, body, size, opts)
}

func (s *stubStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *stubStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return s.ObjectStore.GetRange(ctx, key, offset, length)
}

func (s *stubStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.head != nil {
		return s.head(ctx, key)
	}
	return s.ObjectStore.Head(ctx, key)
}

type errorBody struct {
	readErr  error
	closeErr error
	closes   int
}

func (r *errorBody) Read(p []byte) (int, error) {
	return copy(p, "abc"), r.readErr
}

func (r *errorBody) Close() error {
	r.closes++
	return r.closeErr
}

func newStore(t *testing.T, inner storage.ObjectStore) *metrics.Store {
	t.Helper()
	store, err := metrics.NewStore(inner)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func scrape(ctx context.Context, handler http.Handler) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	return response
}

func assertSample(t *testing.T, handler http.Handler, name string, want float64) {
	t.Helper()
	for line := range strings.SplitSeq(scrape(t.Context(), handler).Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			got, err := strconv.ParseFloat(value, 64)
			if err != nil || got != want {
				t.Fatalf("metric %s = %q, want %g (parse error %v)", name, value, want, err)
			}
			return
		}
	}
	t.Fatalf("metric %s was not exported", name)
}
