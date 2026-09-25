package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memoryObject struct {
	data         []byte
	version      Version
	lastModified time.Time
}

// MemoryStore is a concurrency-safe ObjectStore for tests and local development.
// It is never an authoritative production backend.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
	next    uint64
}

// NewMemoryStore constructs an empty in-memory object store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string]memoryObject)}
}

// Put atomically applies a conditional write.
func (s *MemoryStore) Put(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
	opts PutOptions,
) (ObjectInfo, error) {
	if body == nil {
		return ObjectInfo{}, fmt.Errorf("put %q: body is required", key)
	}
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, fmt.Errorf("put %q: %w", key, err)
	}
	if size < 0 {
		return ObjectInfo{}, fmt.Errorf("put %q: negative size", key)
	}
	if opts.IfMatch != "" && opts.IfNoneMatch {
		return ObjectInfo{}, fmt.Errorf("put %q: conflicting preconditions", key)
	}
	data, err := readExactly(ctx, body, size)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("put %q: %w", key, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.objects[key]
	if opts.IfNoneMatch && exists {
		return ObjectInfo{}, fmt.Errorf("put %q: %w", key, ErrAlreadyExists)
	}
	if opts.IfMatch != "" && (!exists || current.version != opts.IfMatch) {
		return ObjectInfo{}, fmt.Errorf("put %q: %w", key, ErrPreconditionFailed)
	}

	s.next++
	object := memoryObject{
		data:         slices.Clone(data),
		version:      Version(strconv.FormatUint(s.next, 10)),
		lastModified: time.Now().UTC(),
	}
	s.objects[key] = object

	return objectInfo(key, object), nil
}

// Get returns a stable snapshot of an object.
func (s *MemoryStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get %q: %w", key, err)
	}
	if err := validateKey(key); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get %q: %w", key, err)
	}

	s.mu.RLock()
	object, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, ObjectInfo{}, fmt.Errorf("get %q: %w", key, ErrNotFound)
	}

	data := slices.Clone(object.data)
	return io.NopCloser(bytes.NewReader(data)), objectInfo(key, object), nil
}

// GetRange returns at most length bytes beginning at offset.
func (s *MemoryStore) GetRange(
	ctx context.Context,
	key string,
	offset, length int64,
) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get range %q: %w", key, err)
	}
	if err := validateKey(key); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get range %q: %w", key, err)
	}
	if offset < 0 || length <= 0 {
		return nil, ObjectInfo{}, fmt.Errorf("get range %q: %w", key, ErrInvalidRange)
	}

	s.mu.RLock()
	object, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, ObjectInfo{}, fmt.Errorf("get range %q: %w", key, ErrNotFound)
	}
	if offset >= int64(len(object.data)) {
		return nil, ObjectInfo{}, fmt.Errorf("get range %q: %w", key, ErrInvalidRange)
	}

	end := int64(len(object.data))
	if length < end-offset {
		end = offset + length
	}
	data := slices.Clone(object.data[offset:end])
	return io.NopCloser(bytes.NewReader(data)), objectInfo(key, object), nil
}

// Head returns object metadata.
func (s *MemoryStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, err)
	}

	s.mu.RLock()
	object, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return ObjectInfo{}, fmt.Errorf("head %q: %w", key, ErrNotFound)
	}

	return objectInfo(key, object), nil
}

// Delete removes an object, optionally under an If-Match precondition.
func (s *MemoryStore) Delete(ctx context.Context, key string, ifMatch Version) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	if err := validateKey(key); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.objects[key]
	if !exists {
		return fmt.Errorf("delete %q: %w", key, ErrNotFound)
	}
	if ifMatch != "" && current.version != ifMatch {
		return fmt.Errorf("delete %q: %w", key, ErrPreconditionFailed)
	}
	delete(s.objects, key)

	return nil
}

// List returns a key-sorted snapshot under prefix.
func (s *MemoryStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
	if err := ValidatePrefix(prefix); err != nil {
		return nil, fmt.Errorf("list %q: %w", prefix, errInvalidKey)
	}

	s.mu.RLock()
	objects := make([]ObjectInfo, 0)
	for key, object := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, objectInfo(key, object))
		}
	}
	s.mu.RUnlock()

	slices.SortFunc(objects, func(left, right ObjectInfo) int {
		return strings.Compare(left.Key, right.Key)
	})

	return objects, nil
}

// ListPage scans the in-memory index while retaining only one page and a lookahead key.
func (s *MemoryStore) ListPage(ctx context.Context, prefix, after string, limit int) (ObjectPage, error) {
	if err := ctx.Err(); err != nil {
		return ObjectPage{}, fmt.Errorf("list page %q: %w", prefix, err)
	}
	if err := ValidateListPage(prefix, after, limit); err != nil {
		return ObjectPage{}, fmt.Errorf("list page %q: %w", prefix, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, limit+1)
	for key := range s.objects {
		if err := ctx.Err(); err != nil {
			return ObjectPage{}, fmt.Errorf("list page %q: %w", prefix, err)
		}
		if key <= after || !strings.HasPrefix(key, prefix) {
			continue
		}
		position, _ := slices.BinarySearch(keys, key)
		if position > limit {
			continue
		}
		if len(keys) < limit+1 {
			keys = append(keys, "")
		}
		copy(keys[position+1:], keys[position:])
		keys[position] = key
	}
	page := ObjectPage{Objects: make([]ObjectInfo, 0, min(len(keys), limit))}
	if len(keys) > limit {
		keys = keys[:limit]
		page.NextAfter = keys[len(keys)-1]
	}
	for _, key := range keys {
		page.Objects = append(page.Objects, objectInfo(key, s.objects[key]))
	}
	return page, nil
}

func readExactly(ctx context.Context, body io.Reader, size int64) ([]byte, error) {
	limited := io.LimitReader(&contextReader{ctx: ctx, reader: body}, size+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("body size is %d, expected %d", len(data), size)
	}

	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	return r.reader.Read(buffer)
}

func objectInfo(key string, object memoryObject) ObjectInfo {
	return ObjectInfo{
		Key:          key,
		Size:         int64(len(object.data)),
		Version:      object.version,
		LastModified: object.lastModified,
	}
}

var _ ObjectStore = (*MemoryStore)(nil)
