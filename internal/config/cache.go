package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

const DefaultCacheMemoryBytes = int64(256 << 20)

// Cache bounds verified immutable content shared across serving requests.
// Zero disables a tier. Maintenance commands never use the serving cache.
type Cache struct {
	MemoryBytes int64
	DiskBytes   int64
	Directory   string
}

func loadCache(lookup LookupEnv) (Cache, error) {
	memory, err := cacheBytesValue(lookup, "GITONE_CACHE_MEMORY_BYTES", DefaultCacheMemoryBytes)
	if err != nil {
		return Cache{}, err
	}
	disk, err := cacheBytesValue(lookup, "GITONE_CACHE_DISK_BYTES", 0)
	if err != nil {
		return Cache{}, err
	}
	directory, _ := lookup("GITONE_CACHE_DIRECTORY")
	return Cache{MemoryBytes: memory, DiskBytes: disk, Directory: strings.TrimSpace(directory)}, nil
}

func cacheBytesValue(lookup LookupEnv, key string, fallback int64) (int64, error) {
	input, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := parseBytes(strings.TrimSpace(input))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a nonnegative byte size", key)
	}
	return parsed, nil
}

func (c Cache) validate() error {
	if c.MemoryBytes < 0 || c.DiskBytes < 0 {
		return fmt.Errorf("config: cache byte limits must be nonnegative")
	}
	if c.Directory != "" && !filepath.IsAbs(c.Directory) {
		return fmt.Errorf("config: cache directory must be an absolute path")
	}
	if c.DiskBytes > 0 && c.Directory == "" {
		return fmt.Errorf("config: a cache directory is required when disk caching is enabled")
	}
	return nil
}
