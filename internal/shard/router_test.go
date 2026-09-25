package shard

import (
	"errors"
	"math"
	"testing"
)

func TestNewRouterValidatesDependencies(t *testing.T) {
	t.Parallel()

	parser := newTestParser(t)
	if _, err := NewRouter(0, parser); !errors.Is(err, ErrInvalidShardCount) {
		t.Fatalf("NewRouter(0) error = %v, expected ErrInvalidShardCount", err)
	}
	if _, err := NewRouter(1, nil); !errors.Is(err, ErrMissingParser) {
		t.Fatalf("NewRouter(nil parser) error = %v, expected ErrMissingParser", err)
	}
}

func TestRouterOwnerUsesXXHashModuloShardCount(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, 512)
	tests := []struct {
		name     string
		topLevel string
		expected ShardID
	}{
		{name: "alice", topLevel: "alice", expected: 73},
		{name: "acme", topLevel: "acme", expected: 12},
		{name: "owner above uint8", topLevel: "hello", expected: 419},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			actual, err := router.Owner(test.topLevel)
			if err != nil {
				t.Fatalf("Owner() error = %v", err)
			}
			if actual != test.expected {
				t.Fatalf("Owner() = %d, expected %d", actual, test.expected)
			}
		})
	}
}

func TestRouterOwnerShardCountBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		shardCount uint32
	}{
		{name: "single shard", shardCount: 1},
		{name: "maximum shard count", shardCount: math.MaxUint32},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			router := newTestRouter(t, test.shardCount)
			for _, topLevel := range []string{"alice", "acme", "hello"} {
				owner, err := router.Owner(topLevel)
				if err != nil {
					t.Fatalf("Owner(%q): %v", topLevel, err)
				}
				want := Sum64([]byte(topLevel)) % uint64(test.shardCount)
				if uint64(owner) != want || uint32(owner) >= test.shardCount {
					t.Errorf("Owner(%q) = %d, want %d below %d", topLevel, owner, want, test.shardCount)
				}
			}
		})
	}
}

func TestRouterOwnerRejectsNonCanonicalName(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, 256)
	for _, topLevel := range []string{"Alice", "api", "-alice", "alice.git"} {
		t.Run(topLevel, func(t *testing.T) {
			t.Parallel()
			if _, err := router.Owner(topLevel); err == nil {
				t.Fatal("Owner() error = nil, expected validation error")
			}
		})
	}
}

func TestRouterResolve(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, 256)
	route, err := router.Resolve(newRequest(t, "/hello/project.git/info/refs"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if route.Owner != 163 {
		t.Errorf("Owner = %d, expected 163", route.Owner)
	}
	if route.Path.TopLevel != "hello" {
		t.Errorf("TopLevel = %q, expected hello", route.Path.TopLevel)
	}
	if route.Path.Repository != "hello/project" {
		t.Errorf("Repository = %q, expected hello/project", route.Path.Repository)
	}
	if router.ShardCount() != 256 {
		t.Errorf("ShardCount() = %d, expected 256", router.ShardCount())
	}
}

func newTestRouter(t *testing.T, shardCount uint32) *Router {
	t.Helper()
	router, err := NewRouter(shardCount, newTestParser(t))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return router
}
