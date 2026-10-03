package config

import (
	"maps"
	"strings"
	"testing"
)

func TestLoadCache(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		env  map[string]string
		want Cache
	}{
		{name: "defaults", want: Cache{MemoryBytes: 256 << 20}},
		{name: "disabled", env: map[string]string{"GITONE_CACHE_MEMORY_BYTES": "0", "GITONE_CACHE_DISK_BYTES": "0B"}},
		{name: "both tiers", env: map[string]string{
			"GITONE_CACHE_MEMORY_BYTES": "512MiB", "GITONE_CACHE_DISK_BYTES": "16GiB", "GITONE_CACHE_DIRECTORY": "/cache"},
			want: Cache{MemoryBytes: 512 << 20, DiskBytes: 16 << 30, Directory: "/cache"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := baseEnvironment()
			maps.Copy(env, test.env)
			cfg, err := Load(testLookup(env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Cache != test.want {
				t.Fatalf("cache = %+v, want %+v", cfg.Cache, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidCache(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		env   map[string]string
		match string
	}{
		{name: "negative memory", env: map[string]string{"GITONE_CACHE_MEMORY_BYTES": "-1"}, match: "MEMORY_BYTES"},
		{name: "negative disk", env: map[string]string{"GITONE_CACHE_DISK_BYTES": "-1"}, match: "DISK_BYTES"},
		{name: "empty memory", env: map[string]string{"GITONE_CACHE_MEMORY_BYTES": ""}, match: "MEMORY_BYTES"},
		{name: "overflow", env: map[string]string{"GITONE_CACHE_DISK_BYTES": "9223372036854775808"}, match: "DISK_BYTES"},
		{name: "relative directory", env: map[string]string{"GITONE_CACHE_DIRECTORY": "cache"}, match: "absolute path"},
		{name: "missing directory", env: map[string]string{"GITONE_CACHE_DISK_BYTES": "1GiB"}, match: "directory is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := baseEnvironment()
			maps.Copy(env, test.env)
			if _, err := Load(testLookup(env)); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Load error = %v, want %q", err, test.match)
			}
		})
	}
}

func TestCacheValidateRejectsProgrammaticNegativeLimits(t *testing.T) {
	t.Parallel()
	for _, limits := range []Cache{{MemoryBytes: -1}, {DiskBytes: -1}} {
		if err := limits.validate(); err == nil {
			t.Fatalf("accepted negative limits: %+v", limits)
		}
	}
}
