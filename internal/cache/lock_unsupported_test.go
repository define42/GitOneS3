//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !illumos

package cache

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestUnsupportedDiskPlatformKeepsMemoryCacheAvailable(t *testing.T) {
	c := newMemoryCache(t, 1024)
	value, err := c.LoadMemory(t.Context(), "key", func(context.Context) (any, int64, error) {
		return "memory", 6, nil
	})
	if err != nil || value != "memory" {
		t.Fatalf("memory caching = %v, %v", value, err)
	}
	_, err = New(Options{DiskBytes: 4096, Directory: t.TempDir(), Namespace: "test"})
	if err == nil || !strings.Contains(err.Error(), "unsupported on "+runtime.GOOS) {
		t.Fatalf("disk caching error = %v", err)
	}
}
