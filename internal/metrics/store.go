// Package metrics provides bounded storage metrics in Prometheus text format.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

type operation int

const (
	opPut operation = iota
	opGet
	opGetRange
	opHead
	opDelete
	opList
	opListPage
	operationCount
)

var operationNames = [operationCount]string{
	"put", "get", "get_range", "head", "delete", "list", "list_page",
}

const resultCount = 9

var resultNames = [resultCount]string{
	"success", "not_found", "already_exists", "precondition_failed", "conflict",
	"canceled", "deadline_exceeded", "invalid_range", "error",
}

var durationBounds = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type operationStats struct {
	Completed [resultCount]uint64
	Active    int64
	Bytes     uint64
	Buckets   [len(durationBounds)]uint64
	Count     uint64
	Seconds   float64
}

// Store instruments an ObjectStore without retaining object keys or identities.
// GET operations include body consumption and finish when the caller closes the
// body, so streamed read and close errors contribute to the operation result.
// A body that is not closed remains visible as an active operation.
//
// The HTTP handler exposes only aggregate metrics. The caller must provide any
// required authentication or a separate private listener before serving it.
type Store struct {
	inner storage.ObjectStore
	mu    sync.Mutex
	stats [operationCount]operationStats
}

// NewStore wraps a shard's object store with storage metrics.
func NewStore(inner storage.ObjectStore) (*Store, error) {
	if inner == nil {
		return nil, errors.New("metrics object store is required")
	}
	return &Store{inner: inner}, nil
}

// Put measures the call and bytes consumed from body, including retries. Seek
// support is preserved so providers can rewind seekable bodies for retries.
func (s *Store) Put(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
	opts storage.PutOptions,
) (storage.ObjectInfo, error) {
	started := s.start(opPut)
	if body != nil {
		body = wrapReader(body, s, opPut)
	}
	info, err := s.inner.Put(ctx, key, body, size, opts)
	s.finish(opPut, started, err)
	return info, err
}

// Get measures an object read through the returned body's Close call.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	started := s.start(opGet)
	body, info, err := s.inner.Get(ctx, key)
	if err != nil || body == nil {
		s.finish(opGet, started, err)
		return body, info, err
	}
	return &readCloser{inner: body, store: s, op: opGet, started: started}, info, nil
}

// GetRange measures the bytes actually consumed from a ranged read.
func (s *Store) GetRange(
	ctx context.Context,
	key string,
	offset, length int64,
) (io.ReadCloser, storage.ObjectInfo, error) {
	started := s.start(opGetRange)
	body, info, err := s.inner.GetRange(ctx, key, offset, length)
	if err != nil || body == nil {
		s.finish(opGetRange, started, err)
		return body, info, err
	}
	return &readCloser{inner: body, store: s, op: opGetRange, started: started}, info, nil
}

// Head measures an object metadata request.
func (s *Store) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	started := s.start(opHead)
	info, err := s.inner.Head(ctx, key)
	s.finish(opHead, started, err)
	return info, err
}

// Delete measures an object deletion, including conditional conflicts.
func (s *Store) Delete(ctx context.Context, key string, ifMatch storage.Version) error {
	started := s.start(opDelete)
	err := s.inner.Delete(ctx, key, ifMatch)
	s.finish(opDelete, started, err)
	return err
}

// List measures a complete prefix listing.
func (s *Store) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	started := s.start(opList)
	objects, err := s.inner.List(ctx, prefix)
	s.finish(opList, started, err)
	return objects, err
}

// ListPage measures a bounded prefix listing.
func (s *Store) ListPage(ctx context.Context, prefix, after string, limit int) (storage.ObjectPage, error) {
	started := s.start(opListPage)
	page, err := s.inner.ListPage(ctx, prefix, after, limit)
	s.finish(opListPage, started, err)
	return page, err
}

// ServeHTTP exports a consistent snapshot in Prometheus text exposition format.
//
// Error rate: sum(rate(gitone_storage_operations_total{result="error"}[5m])).
// P99 latency: histogram_quantile(0.99,
// sum by (operation, le) (rate(gitone_storage_operation_duration_seconds_bucket[5m]))).
func (s *Store) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodHead {
		response.WriteHeader(http.StatusOK)
		return
	}
	s.mu.Lock()
	stats := s.stats
	s.mu.Unlock()

	var text strings.Builder
	text.WriteString("# HELP gitone_storage_operations_total Completed object store operations by result.\n")
	text.WriteString("# TYPE gitone_storage_operations_total counter\n")
	for op, stat := range stats {
		for result, count := range stat.Completed {
			fmt.Fprintf(&text, "gitone_storage_operations_total{operation=%q,result=%q} %d\n",
				operationNames[op], resultNames[result], count)
		}
	}
	text.WriteString("# HELP gitone_storage_active_operations In-flight operations, including open read bodies.\n")
	text.WriteString("# TYPE gitone_storage_active_operations gauge\n")
	for op, stat := range stats {
		fmt.Fprintf(&text, "gitone_storage_active_operations{operation=%q} %d\n", operationNames[op], stat.Active)
	}
	text.WriteString("# HELP gitone_storage_transferred_bytes_total Bytes consumed from upload and download streams, including upload retries.\n")
	text.WriteString("# TYPE gitone_storage_transferred_bytes_total counter\n")
	for _, op := range []operation{opPut, opGet, opGetRange} {
		fmt.Fprintf(&text, "gitone_storage_transferred_bytes_total{operation=%q} %d\n", operationNames[op], stats[op].Bytes)
	}
	text.WriteString("# HELP gitone_storage_operation_duration_seconds Completed operation duration including read body lifetime.\n")
	text.WriteString("# TYPE gitone_storage_operation_duration_seconds histogram\n")
	for op, stat := range stats {
		for bucket, bound := range durationBounds {
			fmt.Fprintf(&text, "gitone_storage_operation_duration_seconds_bucket{operation=%q,le=\"%g\"} %d\n",
				operationNames[op], bound, stat.Buckets[bucket])
		}
		fmt.Fprintf(&text, "gitone_storage_operation_duration_seconds_bucket{operation=%q,le=\"+Inf\"} %d\n",
			operationNames[op], stat.Count)
		fmt.Fprintf(&text, "gitone_storage_operation_duration_seconds_sum{operation=%q} %g\n", operationNames[op], stat.Seconds)
		fmt.Fprintf(&text, "gitone_storage_operation_duration_seconds_count{operation=%q} %d\n", operationNames[op], stat.Count)
	}
	_, _ = io.WriteString(response, text.String())
}

func (s *Store) start(op operation) time.Time {
	started := time.Now()
	s.mu.Lock()
	s.stats[op].Active++
	s.mu.Unlock()
	return started
}

func (s *Store) finish(op operation, started time.Time, err error) {
	seconds := time.Since(started).Seconds()
	result := classifyResult(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	stat := &s.stats[op]
	stat.Active--
	stat.Completed[result]++
	stat.Count++
	stat.Seconds += seconds
	for bucket, bound := range durationBounds {
		if seconds <= bound {
			stat.Buckets[bucket]++
		}
	}
}

func (s *Store) addBytes(op operation, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.stats[op].Bytes += uint64(n)
	s.mu.Unlock()
}

func classifyResult(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, storage.ErrNotFound):
		return 1
	case errors.Is(err, storage.ErrAlreadyExists):
		return 2
	case errors.Is(err, storage.ErrPreconditionFailed):
		return 3
	case errors.Is(err, storage.ErrConditionalConflict):
		return 4
	case errors.Is(err, context.Canceled):
		return 5
	case errors.Is(err, context.DeadlineExceeded):
		return 6
	case errors.Is(err, storage.ErrInvalidRange):
		return 7
	default:
		return 8
	}
}

type reader struct {
	inner io.Reader
	store *Store
	op    operation
}

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	r.store.addBytes(r.op, n)
	return n, err
}

func wrapReader(inner io.Reader, store *Store, op operation) io.Reader {
	counted := &reader{inner: inner, store: store, op: op}
	seeker, seekable := inner.(io.Seeker)
	closer, closable := inner.(io.Closer)
	switch {
	case seekable && closable:
		return struct {
			io.Reader
			io.Seeker
			io.Closer
		}{counted, seeker, closer}
	case seekable:
		return struct {
			io.Reader
			io.Seeker
		}{counted, seeker}
	case closable:
		return struct {
			io.Reader
			io.Closer
		}{counted, closer}
	default:
		return counted
	}
}

type readCloser struct {
	inner    io.ReadCloser
	store    *Store
	op       operation
	started  time.Time
	once     sync.Once
	mu       sync.Mutex
	readErr  error
	closeErr error
}

func (r *readCloser) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	r.store.addBytes(r.op, n)
	if err != nil && err != io.EOF {
		r.mu.Lock()
		if r.readErr == nil {
			r.readErr = err
		}
		r.mu.Unlock()
	}
	return n, err
}

func (r *readCloser) Close() error {
	r.once.Do(func() {
		r.closeErr = r.inner.Close()
		r.mu.Lock()
		readErr := r.readErr
		r.mu.Unlock()
		r.store.finish(r.op, r.started, errors.Join(readErr, r.closeErr))
	})
	return r.closeErr
}

var _ storage.ObjectStore = (*Store)(nil)
var _ http.Handler = (*Store)(nil)
