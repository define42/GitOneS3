package cache

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func newMemoryCache(t *testing.T, limit int64) *Cache {
	t.Helper()
	c, err := New(Options{MemoryBytes: limit, Namespace: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestLoadMemoryLRUAndReadThrough(t *testing.T) {
	t.Parallel()
	const limit = 2 * (memoryEntryOverhead + 5)
	c := newMemoryCache(t, limit)
	loads := 0
	load := func(key string, size int64) {
		t.Helper()
		value, err := c.LoadMemory(t.Context(), key, func(context.Context) (any, int64, error) {
			loads++
			return key, size, nil
		})
		if err != nil || value != key {
			t.Fatalf("load %s = %v, %v", key, value, err)
		}
	}
	load("a", 4)
	load("b", 4)
	load("a", 4)
	load("c", 4)
	if loads != 3 {
		t.Fatalf("loads=%d", loads)
	}
	load("a", 4)
	load("b", 4)
	if loads != 4 {
		t.Fatalf("LRU loads=%d", loads)
	}
	load("large", limit+1)
	load("large", limit+1)
	if loads != 6 {
		t.Fatalf("oversized values retained: loads=%d", loads)
	}
	if c.MemoryLimit() != limit {
		t.Fatal("incorrect memory limit")
	}
	zero := newMemoryCache(t, 0)
	for range 2 {
		value, err := zero.LoadMemory(t.Context(), "a", func(context.Context) (any, int64, error) { return "uncached", 4, nil })
		if err != nil || value != "uncached" {
			t.Fatalf("disabled = %v, %v", value, err)
		}
	}
}

func TestLoadMemoryCoalescingCancellationAndRecursiveLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newMemoryCache(t, 100)
		var calls atomic.Int32
		release := make(chan struct{})
		load := func(ctx context.Context) (any, int64, error) {
			calls.Add(1)
			<-release
			_, err := c.LoadMemory(ctx, "nested", func(context.Context) (any, int64, error) { return "nested", 1, nil })
			return "shared", 5, err
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				value, err := c.LoadMemory(t.Context(), "shared", load)
				if err != nil || value != "shared" {
					t.Errorf("load=%v, %v", value, err)
				}
			})
		}
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		cancelled := make(chan error, 1)
		go func() { _, err := c.LoadMemory(ctx, "shared", load); cancelled <- err }()
		synctest.Wait()
		cancel()
		if err := <-cancelled; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter: %v", err)
		}
		close(release)
		wg.Wait()
		if calls.Load() != 1 {
			t.Fatalf("fills=%d", calls.Load())
		}
	})
}

func TestLoadMemoryCancelledLeaderAndErrorDoNotPublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newMemoryCache(t, 100)
		ctx, cancel := context.WithCancel(t.Context())
		started := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			_, err := c.LoadMemory(ctx, "k", func(ctx context.Context) (any, int64, error) {
				close(started)
				<-ctx.Done()
				return "must not publish", 1, nil
			})
			errs <- err
		}()
		<-started
		go func() { _, err := c.LoadMemory(t.Context(), "k", nil); errs <- err }()
		synctest.Wait()
		cancel()
		for range 2 {
			if err := <-errs; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		}
		want := errors.New("load failed")
		if _, err := c.LoadMemory(t.Context(), "k", func(context.Context) (any, int64, error) { return nil, 0, want }); !errors.Is(err, want) {
			t.Fatal(err)
		}
		got, err := c.LoadMemory(t.Context(), "k", func(context.Context) (any, int64, error) { return "fresh", 1, nil })
		if err != nil || got != "fresh" {
			t.Fatalf("retry=%v, %v", got, err)
		}
	})
}

func TestCloseCancelsFillsAndMetricsHideKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newMemoryCache(t, 100)
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := c.LoadMemory(t.Context(), "private-repository-name", func(ctx context.Context) (any, int64, error) {
				close(started)
				<-ctx.Done()
				return nil, 0, ctx.Err()
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
		if _, err := c.LoadMemory(t.Context(), "k", nil); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		var metrics bytes.Buffer
		c.WritePrometheus(&metrics)
		if strings.Contains(metrics.String(), "private-repository-name") {
			t.Fatal("key leaked")
		}
		if !strings.Contains(metrics.String(), "gitone_cache_memory_fills 0\n") {
			t.Fatal(metrics.String())
		}
	})
}

func TestLoaderPanicReleasesClose(t *testing.T) {
	c := newMemoryCache(t, 100)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("missing panic")
			}
		}()
		_, _ = c.LoadMemory(t.Context(), "panic", func(context.Context) (any, int64, error) { panic("loader") })
	}()
	value, err := c.LoadMemory(t.Context(), "panic", func(context.Context) (any, int64, error) { return "retry", 1, nil })
	if err != nil || value != "retry" {
		t.Fatal(value, err)
	}
}
