package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type diskEntry struct {
	key      string
	path     string
	checksum string
	size     int64
	charge   int64
	pins     int
	retired  bool
	lru      *list.Element
	verified os.FileInfo
	checking chan struct{}
}

// File owns an independent read position and pins its immutable cache entry.
// Close releases the pin exactly once, including after the cache has closed.
type File struct {
	file  *os.File
	cache *Cache
	entry *diskEntry
	once  sync.Once
	err   error
}

func (f *File) Read(p []byte) (int, error)                { return f.file.Read(p) }
func (f *File) ReadAt(p []byte, off int64) (int, error)   { return f.file.ReadAt(p, off) }
func (f *File) Seek(off int64, whence int) (int64, error) { return f.file.Seek(off, whence) }
func (f *File) Size() int64                               { return f.entry.size }
func (f *File) Close() error {
	f.once.Do(func() { f.err = errors.Join(f.file.Close(), f.cache.unpin(f.entry)) })
	return f.err
}

// MemoryLimit returns the configured retained-value budget.
func (c *Cache) MemoryLimit() int64 { return c.memoryLimit }

// OpenFile opens a completed entry or returns os.ErrNotExist. It checks the
// complete checksum on first open after restart and whenever identity, size or
// modification time changes. The private directory must not be modified by
// other processes. Callers retain their object-level integrity checks.
func (c *Cache) OpenFile(ctx context.Context, key string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = digest(key)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	entry := c.files[key]
	if entry == nil {
		c.metrics.diskMisses++
		c.mu.Unlock()
		return nil, os.ErrNotExist
	}
	entry.pins++
	c.pins++
	c.diskLRU.MoveToFront(entry.lru)
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	file, err := os.Open(entry.path) // #nosec G304 -- Generated within the private namespace directory.
	if err == nil {
		err = c.verifyFile(ctx, entry, file)
	}
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		if ctx.Err() == nil {
			c.mu.Lock()
			c.retire(entry)
			c.metrics.corruptions++
			c.mu.Unlock()
		}
		_ = c.unpin(entry)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, os.ErrNotExist
	}
	c.mu.Lock()
	closed, retired := c.closed, entry.retired
	if !closed && !retired {
		c.metrics.diskHits++
	}
	c.mu.Unlock()
	if closed || retired {
		_ = file.Close()
		_ = c.unpin(entry)
		if closed {
			return nil, ErrClosed
		}
		return nil, os.ErrNotExist
	}
	return &File{file: file, cache: c, entry: entry}, nil
}

func (c *Cache) verifyFile(ctx context.Context, entry *diskEntry, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.size {
			return errors.New("cache: changed file")
		}
		c.mu.Lock()
		if entry.retired {
			c.mu.Unlock()
			return errors.New("cache: retired file")
		}
		if sameFile(entry.verified, info) {
			c.mu.Unlock()
			return nil
		}
		if checking := entry.checking; checking != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-checking:
				continue
			}
		}
		entry.checking = make(chan struct{})
		c.mu.Unlock()
		digest := sha256.New()
		_, err = io.Copy(digest, &contextReader{ctx: ctx, reader: io.NewSectionReader(file, 0, entry.size)})
		after, statErr := file.Stat()
		if err == nil && (statErr != nil || !sameFile(info, after) || hex.EncodeToString(digest.Sum(nil)) != entry.checksum) {
			err = errors.New("cache: file checksum mismatch")
		}
		c.mu.Lock()
		if err == nil {
			entry.verified = after
		}
		close(entry.checking)
		entry.checking = nil
		c.mu.Unlock()
		return err
	}
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// LoadFile returns a pinned file, coalescing builds for the same key. maxBytes
// is reserved before a callback starts; an exceeding write fails with
// ErrCapacity. Reservations and pinned entries remain inside the disk budget.
// Entries/reservations are rounded up to 4 KiB, including empty files. Filesystem
// metadata and larger filesystem block sizes need deployment-level headroom.
func (c *Cache) LoadFile(ctx context.Context, key string, maxBytes int64, build func(context.Context, io.Writer) error) (*File, error) {
	charge, err := diskCharge(maxBytes)
	if err != nil {
		return nil, err
	}
	for {
		file, err := c.OpenFile(ctx, key)
		if err == nil {
			if file.Size() > maxBytes {
				_ = file.Close()
				return nil, ErrCapacity
			}
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		keyHash := digest(key)
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if c.files[keyHash] != nil {
			c.mu.Unlock()
			continue
		}
		if pending := c.diskFills[keyHash]; pending != nil {
			c.metrics.coalesced++
			c.mu.Unlock()
			if _, err := await(ctx, pending); err != nil {
				return nil, err
			}
			continue
		}
		if build == nil {
			c.mu.Unlock()
			return nil, errors.New("cache: nil file builder")
		}
		if err := c.makeDiskRoom(charge); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		fillCtx, cancel := context.WithCancel(ctx)
		pending := &flight{done: make(chan struct{}), cancel: cancel}
		c.diskFills[keyHash] = pending
		c.metrics.diskBuilds++
		c.diskBytes += charge
		c.diskEntries++
		c.wg.Add(1)
		c.mu.Unlock()
		err = func() error {
			defer cancel()
			defer c.wg.Done()
			return c.buildFile(fillCtx, keyHash, maxBytes, charge, build, pending)
		}()
		if err != nil {
			return nil, err
		}
	}
}

func (c *Cache) buildFile(ctx context.Context, key string, maxBytes, reserved int64, build func(context.Context, io.Writer) error, pending *flight) (err error) {
	var entry *diskEntry
	defer func() {
		panicValue := recover()
		if panicValue != nil {
			err = errors.New("cache: file builder panicked")
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.diskBytes -= reserved
		c.diskEntries--
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil && c.closed {
			err = ErrClosed
		}
		if err == nil {
			c.diskBytes += entry.charge
			c.diskEntries++
			entry.lru = c.diskLRU.PushFront(entry)
			c.files[key] = entry
		} else {
			c.metrics.fillErrors++
			if entry != nil {
				_ = os.Remove(entry.path)
			}
		}
		pending.err = err
		delete(c.diskFills, key)
		close(pending.done)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	file, err := os.CreateTemp(c.directory, ".fill-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(temp) }()
	writer := &boundedWriter{ctx: ctx, writer: file, hash: sha256.New(), maxBytes: maxBytes}
	if err := build(ctx, writer); err != nil {
		return err
	}
	if writer.err != nil {
		return writer.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	checksum := hex.EncodeToString(writer.hash.Sum(nil))
	name := key + "." + checksum + "." + strings.TrimPrefix(filepath.Base(temp), ".fill-") + ".cache"
	path := filepath.Join(c.directory, name)
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	charge, _ := diskCharge(writer.bytes)
	entry = &diskEntry{key: key, path: path, checksum: checksum, size: writer.bytes, charge: charge}
	return nil
}

type boundedWriter struct {
	ctx             context.Context
	writer          io.Writer
	hash            hash.Hash
	maxBytes, bytes int64
	err             error
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.maxBytes-w.bytes {
		w.err = ErrCapacity
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	_, _ = w.hash.Write(p[:n])
	w.bytes += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func diskCharge(size int64) (int64, error) {
	if size < 0 || size > math.MaxInt64-4095 {
		return 0, ErrCapacity
	}
	return max(int64(4096), (size+4095)/4096*4096), nil
}

// Caller holds mu. Metadata removals are serialized, but payload reads and
// checksum verification never hold the cache mutex.
func (c *Cache) makeDiskRoom(charge int64) error {
	if charge > c.diskLimit {
		return ErrCapacity
	}
	for c.diskBytes > c.diskLimit-charge || c.diskEntries >= c.entryLimit {
		var victim *diskEntry
		for item := c.diskLRU.Back(); item != nil; item = item.Prev() {
			entry := item.Value.(*diskEntry)
			if entry.pins == 0 {
				victim = entry
				break
			}
		}
		if victim == nil {
			return ErrCapacity
		}
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict cache file: %w", err)
		}
		c.retire(victim)
		c.diskBytes -= victim.charge
		c.diskEntries--
		c.metrics.diskEvictions++
	}
	return nil
}

func (c *Cache) retire(entry *diskEntry) {
	if entry.retired {
		return
	}
	entry.retired = true
	delete(c.files, entry.key)
	c.diskLRU.Remove(entry.lru)
}

func (c *Cache) unpin(entry *diskEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.pins--
	c.pins--
	var err error
	if entry.retired && entry.pins == 0 {
		err = os.Remove(entry.path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err == nil {
			c.diskBytes -= entry.charge
			c.diskEntries--
		}
	}
	if c.closed && c.pins == 0 && c.lock != nil {
		select {
		case <-c.closeDone:
			err = errors.Join(err, c.lock.Close())
			c.lock = nil
		default:
		}
	}
	return err
}

func (c *Cache) recoverFiles() (err error) {
	directory, err := os.Open(c.directory) // #nosec G304 -- Namespace directory is derived from trusted configuration.
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	// Read bounded batches and enforce capacity while recovering. Directory
	// enumeration order approximates startup recency; subsequent accesses use LRU.
	for {
		files, readErr := directory.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, file := range files {
			if err := c.recoverFile(file); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func (c *Cache) recoverFile(file os.DirEntry) error {
	name := file.Name()
	if name == ".lock" {
		return nil
	}
	path := filepath.Join(c.directory, name)
	parts := strings.Split(name, ".")
	info, err := file.Info()
	if err != nil {
		return err
	}
	valid := len(parts) == 4 && validDigest(parts[0]) && validDigest(parts[1]) && parts[2] != "" && parts[3] == "cache"
	if !valid || !info.Mode().IsRegular() {
		if !file.IsDir() {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		return nil
	}
	charge, err := diskCharge(info.Size())
	if err != nil || charge > c.diskLimit {
		if err := os.Remove(path); err != nil {
			return err
		}
		return nil
	}
	entry := &diskEntry{key: parts[0], path: path, checksum: parts[1], size: info.Size(), charge: charge}
	if previous := c.files[entry.key]; previous != nil {
		if err := os.Remove(previous.path); err != nil {
			return err
		}
		c.retire(previous)
		c.diskBytes -= previous.charge
		c.diskEntries--
	}
	if err := c.makeDiskRoom(entry.charge); err != nil {
		return err
	}
	entry.lru = c.diskLRU.PushFront(entry)
	c.files[entry.key] = entry
	c.diskBytes += entry.charge
	c.diskEntries++
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
