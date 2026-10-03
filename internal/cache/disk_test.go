//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || illumos

package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func newDiskCache(t *testing.T, limit int64) (*Cache, Options) {
	t.Helper()
	options := Options{DiskBytes: limit, Directory: t.TempDir(), Namespace: "bucket-one"}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, options
}

func storeFile(t *testing.T, c *Cache, key, value string) *File {
	t.Helper()
	file, err := c.LoadFile(t.Context(), key, int64(len(value)), func(_ context.Context, writer io.Writer) error {
		_, err := io.WriteString(writer, value)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func readFile(t *testing.T, file *File, want string) {
	t.Helper()
	value, err := io.ReadAll(file)
	if err != nil || string(value) != want {
		t.Fatalf("read=%q, %v; want %q", value, err, want)
	}
	if file.Size() != int64(len(want)) {
		t.Fatal("wrong size")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDiskFilesLRUPinsAndIndependentReaders(t *testing.T) {
	t.Parallel()
	c, _ := newDiskCache(t, 8192)
	a := storeFile(t, c, "a", "alpha")
	b := storeFile(t, c, "b", "beta")
	if _, err := c.LoadFile(t.Context(), "c", 5, func(context.Context, io.Writer) error { t.Error("builder ran without capacity"); return nil }); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	readFile(t, storeFile(t, c, "c", "gamma"), "gamma")
	if _, err := c.OpenFile(t.Context(), "b"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evicted b: %v", err)
	}
	a2, err := c.OpenFile(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := a2.ReadAt(buf, 1); err != nil || string(buf) != "lp" {
		t.Fatalf("ReadAt: %q %v", buf, err)
	}
	if _, err := a2.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(a2)
	if err != nil || string(data) != "pha" {
		t.Fatal(string(data), err)
	}
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
	readFile(t, a, "alpha")
}

func TestDiskRestartCleanupCorruptionAndNamespaceIsolation(t *testing.T) {
	t.Parallel()
	c, options := newDiskCache(t, 16384)
	readFile(t, storeFile(t, c, "a", "alpha"), "alpha")
	path := c.files[digest("a")].path
	if err := os.WriteFile(filepath.Join(c.directory, ".fill-crashed"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	file, err := restarted.OpenFile(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	readFile(t, file, "alpha")
	if _, err := os.Stat(filepath.Join(restarted.directory, ".fill-crashed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished file survived: %v", err)
	}
	if err := os.WriteFile(path, []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.OpenFile(t.Context(), "a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corruption=%v", err)
	}
	readFile(t, storeFile(t, restarted, "a", "fresh"), "fresh")
	options.Namespace = "bucket-two"
	other, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.OpenFile(t.Context(), "a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("namespace collision", err)
	}
}

func TestDiskFillCoalescingReservationsAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newDiskCache(t, 8192)
		var calls atomic.Int32
		release := make(chan struct{})
		build := func(_ context.Context, writer io.Writer) error {
			calls.Add(1)
			<-release
			_, err := io.WriteString(writer, "shared")
			return err
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				file, err := c.LoadFile(t.Context(), "shared", 8192, build)
				if err != nil {
					t.Error(err)
					return
				}
				readFile(t, file, "shared")
			})
		}
		synctest.Wait()
		if _, err := c.LoadFile(t.Context(), "other", 1, build); !errors.Is(err, ErrCapacity) {
			t.Fatal("reservation ignored", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() { _, err := c.LoadFile(ctx, "shared", 8192, build); errCh <- err }()
		synctest.Wait()
		cancel()
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		wg.Wait()
		if calls.Load() != 1 {
			t.Fatalf("builds=%d", calls.Load())
		}
	})
}

func TestDiskFailedOrCancelledFillCannotPublish(t *testing.T) {
	t.Parallel()
	c, _ := newDiskCache(t, 8192)
	for _, tc := range []struct {
		name   string
		cancel bool
	}{{"oversize", false}, {"cancelled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, err := c.LoadFile(ctx, tc.name, 4, func(_ context.Context, writer io.Writer) error {
				if tc.cancel {
					cancel()
				}
				_, _ = io.WriteString(writer, "exceeds")
				return nil
			})
			want := ErrCapacity
			if tc.cancel {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
			if _, err := c.OpenFile(t.Context(), tc.name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published failed fill: %v", err)
			}
		})
	}
	readFile(t, storeFile(t, c, "retry", "works"), "works")
	files, err := os.ReadDir(c.directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".fill-") {
			t.Error("temporary file survived")
		}
	}
}

func TestDiskOwnershipAndClosePreservePinnedReaders(t *testing.T) {
	t.Parallel()
	c, options := newDiskCache(t, 8192)
	file := storeFile(t, c, "a", "alpha")
	if other, err := New(options); err == nil {
		_ = other.Close()
		t.Fatal("simultaneous directory ownership")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := New(options); err == nil {
		_ = other.Close()
		t.Fatal("lock released while readers pinned")
	}
	readFile(t, file, "alpha")
	other, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	reloaded, err := other.OpenFile(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	readFile(t, reloaded, "alpha")
}

func TestDiskEntryLimitIncludesPinnedFilesAndFills(t *testing.T) {
	t.Parallel()
	c, _ := newDiskCache(t, 1<<20)
	// Exercise the fixed entry-count policy at a small scale.
	c.entryLimit = 2
	a := storeFile(t, c, "a", "alpha")
	b := storeFile(t, c, "b", "beta")
	if _, err := c.LoadFile(t.Context(), "c", 1, func(context.Context, io.Writer) error {
		t.Error("entry limit did not reserve metadata capacity")
		return nil
	}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("entry capacity: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	readFile(t, storeFile(t, c, "c", "gamma"), "gamma")
	if _, err := c.OpenFile(t.Context(), "b"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("entry limit did not evict", err)
	}
	readFile(t, a, "alpha")
}

func TestDiskRestartEnforcesReducedBudgetAndVerifiesFirstRead(t *testing.T) {
	t.Parallel()
	c, options := newDiskCache(t, 4096*10)
	for i := range 10 {
		key := fmt.Sprint(i)
		readFile(t, storeFile(t, c, key, key), key)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	options.DiskBytes = 8192
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	files, err := os.ReadDir(restarted.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("restart retained %d files including lock, want 3", len(files))
	}
	var key string
	var entry *diskEntry
	for i := range 10 {
		key = fmt.Sprint(i)
		entry = restarted.files[digest(key)]
		if entry != nil {
			break
		}
	}
	if entry == nil {
		t.Fatal("no recovered entry")
	}
	// It has not been opened since recovery, so first read must hash its payload.
	if err := os.WriteFile(entry.path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.OpenFile(t.Context(), key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted corrupt persisted entry: %v", err)
	}
	readFile(t, storeFile(t, restarted, key, "reloaded"), "reloaded")
}

func TestCloseCancelsDiskBuildAndPanicReleasesReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newDiskCache(t, 8192)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("missing panic")
				}
			}()
			_, _ = c.LoadFile(t.Context(), "panic", 8192, func(context.Context, io.Writer) error { panic("builder") })
		}()
		readFile(t, storeFile(t, c, "retry", "works"), "works")
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := c.LoadFile(t.Context(), "cancel", 8192, func(ctx context.Context, _ io.Writer) error {
				close(started)
				<-ctx.Done()
				return nil
			})
			done <- err
		}()
		<-started
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		files, err := os.ReadDir(c.directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatalf("cancelled build left %d files, want only ownership lock", len(files))
		}
	})
}
