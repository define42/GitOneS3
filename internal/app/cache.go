package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/config"
)

func newRepositoryCache(cfg config.Config) (*cache.Cache, error) {
	namespace, err := repositoryCacheNamespace(cfg)
	if err != nil {
		return nil, err
	}
	result, err := cache.New(cache.Options{
		MemoryBytes: cfg.Cache.MemoryBytes,
		DiskBytes:   cfg.Cache.DiskBytes,
		Directory:   cfg.Cache.Directory,
		Namespace:   namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("create repository cache: %w", err)
	}
	return result, nil
}

func repositoryCacheNamespace(cfg config.Config) (string, error) {
	bucket, err := cfg.BucketFor(cfg.LocalShard)
	if err != nil {
		return "", err
	}
	// Length-delimited JSON keeps endpoint, region and bucket unambiguous.
	// A derived bucket also covers programmatic configs without S3.Bucket set.
	identity, err := json.Marshal([3]string{cfg.S3.Endpoint, cfg.S3.Region, bucket})
	if err != nil {
		return "", fmt.Errorf("encode cache identity: %w", err)
	}
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:]), nil
}
