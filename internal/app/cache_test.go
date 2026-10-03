package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/httpserver"
)

func TestRepositoryCacheNamespaceSeparatesStores(t *testing.T) {
	t.Parallel()
	base := config.Config{ShardCount: 64, LocalShard: 1,
		S3: config.S3{Endpoint: "https://s3.example", Region: "region-a", BucketPrefix: "gitone"}}
	original, err := repositoryCacheNamespace(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*config.Config)
	}{
		{name: "endpoint", change: func(cfg *config.Config) { cfg.S3.Endpoint = "https://other.example" }},
		{name: "region", change: func(cfg *config.Config) { cfg.S3.Region = "region-b" }},
		{name: "bucket prefix", change: func(cfg *config.Config) { cfg.S3.BucketPrefix = "other" }},
		{name: "shard", change: func(cfg *config.Config) { cfg.LocalShard = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			test.change(&cfg)
			got, err := repositoryCacheNamespace(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got == original {
				t.Fatal("different storage identities share a cache namespace")
			}
		})
	}
	base.S3.Bucket = "gitone-01"
	got, err := repositoryCacheNamespace(base)
	if err != nil || got != original {
		t.Fatalf("derived and explicit bucket disagree: namespace=%q, error=%v", got, err)
	}
}

func TestAppCloseReleasesCache(t *testing.T) {
	t.Parallel()
	cfg := config.Config{ShardCount: 1, S3: config.S3{BucketPrefix: "gitone", Region: "test"},
		Cache: config.Cache{MemoryBytes: 1024, DiskBytes: 1024, Directory: t.TempDir()}}
	shared, err := newRepositoryCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{repositoryCache: shared}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.LoadMemory(t.Context(), "closed", func(context.Context) (any, int64, error) {
		t.Error("cache loader ran after app closed")
		return "value", 5, nil
	}); err == nil {
		t.Fatal("closed app cache accepted a load")
	}
	reopened, err := newRepositoryCache(cfg)
	if err != nil {
		t.Fatalf("closed app retained disk cache ownership: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunClosesCacheOnListenerFailure(t *testing.T) {
	t.Parallel()
	cfg := config.Config{ShardCount: 1, S3: config.S3{BucketPrefix: "gitone", Region: "test"},
		Cache: config.Cache{DiskBytes: 1024, Directory: t.TempDir()}}
	shared, err := newRepositoryCache(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := httpserver.New("missing-port", http.NotFoundHandler(), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{server: server, repositoryCache: shared}
	if err := app.Run(t.Context()); err == nil {
		t.Fatal("invalid listener succeeded")
	}
	reopened, err := newRepositoryCache(cfg)
	if err != nil {
		t.Fatalf("failed Run retained cache ownership: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
