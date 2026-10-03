// Package cache provides disposable, bounded caches for verified immutable data.
// It never stores repository authority or permissions. Memory budgets cover
// retained values, not loader temporaries or values retained by callers after
// eviction; callers must bound that work through their own admission controls.
package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var (
	// ErrCapacity means caching is disabled, the item is too large, or all
	// available disk capacity is reserved by fills or pinned readers.
	ErrCapacity = errors.New("cache: capacity unavailable")
	// ErrClosed means the cache is shutting down or has already closed.
	ErrClosed = errors.New("cache: closed")
)

// Options configures independent memory and disk budgets. Namespace must bind
// immutable keys to their authority (for example endpoint, bucket, and region).
// Directory must be absolute when disk caching is enabled. One live Cache owns
// each namespace directory; a process lock prevents competing cleanup/eviction.
type Options struct {
	MemoryBytes int64
	DiskBytes   int64
	Directory   string
	Namespace   string
}

type memoryEntry struct {
	key   string
	value any
	bytes int64
	lru   *list.Element
}

// Includes an entry, its LRU node, map slack and allocator rounding. The caller
// separately reports the value's entire retained object graph.
const memoryEntryOverhead int64 = 256

// MaxDiskEntries bounds the disk index's RAM use independently of payload byte
// capacity. The count includes in-flight builds and retired, still-pinned files.
const MaxDiskEntries = 100000

type flight struct {
	done   chan struct{}
	cancel context.CancelFunc
	value  any
	err    error
}

// Cache is safe for concurrent use. Fill callbacks execute synchronously in the
// first caller. Cancelling that caller can fail its waiters; no background fill
// outlives the owning request. Callbacks must honor context cancellation and must
// not recursively load the same key; different keys may be loaded recursively.
type Cache struct {
	mu sync.Mutex
	wg sync.WaitGroup

	memoryLimit int64
	diskLimit   int64
	memoryBytes int64
	diskBytes   int64
	diskEntries int
	entryLimit  int
	memory      map[string]*memoryEntry
	files       map[string]*diskEntry
	memoryLRU   list.List
	diskLRU     list.List
	memoryFills map[string]*flight
	diskFills   map[string]*flight
	directory   string
	lock        *os.File
	pins        int
	closed      bool
	closeDone   chan struct{}
	closeErr    error
	metrics     counters
}

// New opens the namespace cache and recovers completed disk entries. Incomplete
// temporary files are removed; payload checksums are checked before each open.
func New(options Options) (*Cache, error) {
	if options.MemoryBytes < 0 || options.DiskBytes < 0 || options.Namespace == "" {
		return nil, errors.New("cache: nonnegative budgets and a namespace are required")
	}
	c := &Cache{memoryLimit: options.MemoryBytes, diskLimit: options.DiskBytes, entryLimit: MaxDiskEntries,
		memory: make(map[string]*memoryEntry), files: make(map[string]*diskEntry),
		memoryFills: make(map[string]*flight), diskFills: make(map[string]*flight),
		closeDone: make(chan struct{})}
	if options.DiskBytes == 0 {
		return c, nil
	}
	if !filepath.IsAbs(options.Directory) {
		return nil, errors.New("cache: disk directory must be absolute")
	}
	c.directory = filepath.Join(options.Directory, "v1-"+digest(options.Namespace))
	if err := os.MkdirAll(c.directory, 0700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	info, err := os.Lstat(c.directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("cache: namespace directory must be a real directory")
	}
	if err := os.Chmod(c.directory, 0700); err != nil { // #nosec G302 -- A private directory requires owner execute permission for traversal.
		return nil, err
	}
	if err := c.acquireLock(); err != nil {
		return nil, err
	}
	if err := c.recoverFiles(); err != nil {
		return nil, errors.Join(err, c.lock.Close())
	}
	return c, nil
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// LoadMemory returns a shared immutable value or loads it once for concurrent
// callers. The loader reports a conservative retained size including referenced
// data. Callers must clone anything they mutate. Disabled caching, nonpositive
// sizes and oversized values return the loaded value without retaining it.
func (c *Cache) LoadMemory(ctx context.Context, key string, load func(context.Context) (any, int64, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if entry := c.memory[key]; entry != nil {
		c.memoryLRU.MoveToFront(entry.lru)
		c.metrics.memoryHits++
		c.mu.Unlock()
		return entry.value, nil
	}
	if pending := c.memoryFills[key]; pending != nil {
		c.metrics.coalesced++
		c.mu.Unlock()
		return await(ctx, pending)
	}
	if load == nil {
		c.mu.Unlock()
		return nil, errors.New("cache: nil memory loader")
	}
	fillCtx, cancel := context.WithCancel(ctx)
	pending := &flight{done: make(chan struct{}), cancel: cancel}
	c.memoryFills[key] = pending
	c.metrics.memoryMisses++
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	defer cancel()
	defer func() {
		if value := recover(); value != nil {
			c.mu.Lock()
			if c.memoryFills[key] == pending {
				c.metrics.fillErrors++
				pending.err = errors.New("cache: memory loader panicked")
				delete(c.memoryFills, key)
				close(pending.done)
			}
			c.mu.Unlock()
			panic(value)
		}
	}()
	value, size, err := load(fillCtx)
	if err == nil {
		err = fillCtx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed && err == nil {
		err = ErrClosed
	}
	overheadFits := c.memoryLimit >= memoryEntryOverhead && int64(len(key)) <= c.memoryLimit-memoryEntryOverhead
	if err == nil && size > 0 && overheadFits && size <= c.memoryLimit-memoryEntryOverhead-int64(len(key)) {
		charge := size + memoryEntryOverhead + int64(len(key))
		for c.memoryBytes > c.memoryLimit-charge {
			old := c.memoryLRU.Back().Value.(*memoryEntry)
			delete(c.memory, old.key)
			c.memoryLRU.Remove(old.lru)
			c.memoryBytes -= old.bytes
			c.metrics.memoryEvictions++
		}
		entry := &memoryEntry{key: key, value: value, bytes: charge}
		entry.lru = c.memoryLRU.PushFront(entry)
		c.memory[key] = entry
		c.memoryBytes += charge
	}
	if err == nil {
		pending.value = value
	} else {
		c.metrics.fillErrors++
	}
	pending.err = err
	delete(c.memoryFills, key)
	close(pending.done)
	return pending.value, err
}

func await(ctx context.Context, pending *flight) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-pending.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return pending.value, pending.err
	}
}

// Close rejects new requests, cancels fills and waits for their callbacks.
// Existing File handles remain usable and keep their files pinned. The disk
// ownership lock is released after the last such handle closes. Close preserves
// completed disk entries for a later process and starts no background goroutine.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	for _, pending := range c.memoryFills {
		pending.cancel()
	}
	for _, pending := range c.diskFills {
		pending.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memory = nil
	c.memoryLRU.Init()
	c.memoryBytes = 0
	if c.pins == 0 && c.lock != nil {
		c.closeErr = c.lock.Close()
		c.lock = nil
	}
	close(c.closeDone)
	return c.closeErr
}

// WritePrometheus writes metrics without cache keys, identities, or namespaces.
func (c *Cache) WritePrometheus(w io.Writer) {
	c.mu.Lock()
	m, memoryBytes, diskBytes, pins := c.metrics, c.memoryBytes, c.diskBytes, c.pins
	memoryEntries, diskEntries := len(c.memory), len(c.files)
	memoryFills, diskFills := len(c.memoryFills), len(c.diskFills)
	c.mu.Unlock()
	for _, metric := range []struct {
		name  string
		kind  string
		value any
	}{
		{"memory_hits_total", "counter", m.memoryHits}, {"memory_misses_total", "counter", m.memoryMisses},
		{"disk_hits_total", "counter", m.diskHits}, {"disk_misses_total", "counter", m.diskMisses},
		{"disk_builds_total", "counter", m.diskBuilds},
		{"memory_evictions_total", "counter", m.memoryEvictions}, {"disk_evictions_total", "counter", m.diskEvictions},
		{"coalesced_total", "counter", m.coalesced}, {"fill_errors_total", "counter", m.fillErrors},
		{"corruptions_total", "counter", m.corruptions},
		{"memory_bytes", "gauge", memoryBytes}, {"disk_bytes", "gauge", diskBytes},
		{"memory_entries", "gauge", memoryEntries}, {"disk_entries", "gauge", diskEntries},
		{"memory_fills", "gauge", memoryFills}, {"disk_fills", "gauge", diskFills},
		{"disk_pins", "gauge", pins},
	} {
		_, _ = fmt.Fprintf(w, "# TYPE gitone_cache_%s %s\ngitone_cache_%s %d\n", metric.name, metric.kind, metric.name, metric.value)
	}
}

type counters struct {
	diskBuilds                                     uint64
	memoryHits, memoryMisses, diskHits, diskMisses uint64
	memoryEvictions, diskEvictions, coalesced      uint64
	fillErrors, corruptions                        uint64
}
