package config

import (
	"testing"
	"time"
)

func TestLFSConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, key, value string
		valid            bool
	}{
		{"disabled", "GITONE_LFS_ENABLED", "false", true},
		{"size", "GITONE_LFS_MAX_OBJECT_BYTES", "2GiB", true},
		{"too large", "GITONE_LFS_MAX_OBJECT_BYTES", "65GiB", false},
		{"small quota", "GITONE_LFS_MAX_REPOSITORY_BYTES", "1MiB", false},
		{"maximum quota", "GITONE_LFS_MAX_REPOSITORY_BYTES", "1PiB", true},
		{"excessive quota", "GITONE_LFS_MAX_REPOSITORY_BYTES", "2PiB", false},
		{"negative quota", "GITONE_LFS_MAX_REPOSITORY_BYTES", "-1", false},
		{"concurrency", "GITONE_LFS_MAX_CONCURRENT_TRANSFERS", "3", true},
		{"excessive concurrency", "GITONE_LFS_MAX_CONCURRENT_TRANSFERS", "33", false},
		{"no queue", "GITONE_LFS_MAX_QUEUED_TRANSFERS", "0", true},
		{"negative queue", "GITONE_LFS_MAX_QUEUED_TRANSFERS", "-1", false},
		{"timeout", "GITONE_LFS_TRANSFER_TIMEOUT", "45m", true},
		{"excessive timeout", "GITONE_LFS_TRANSFER_TIMEOUT", "13h", false},
		{"zero timeout", "GITONE_LFS_TRANSFER_TIMEOUT", "0s", false},
		{"queue timeout", "GITONE_LFS_QUEUE_TIMEOUT", "91s", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := baseEnvironment()
			env[test.key] = test.value
			_, err := Load(testLookup(env))
			if (err == nil) != test.valid {
				t.Fatalf("Load() = %v, want valid %t", err, test.valid)
			}
		})
	}
	cfg, err := Load(testLookup(baseEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LFS.Enabled || cfg.LFS.MaxObjectBytes != 1<<30 || cfg.LFS.MaxRepositoryBytes != 10<<30 || cfg.LFS.MaxConcurrentTransfers != 4 || cfg.LFS.MaxQueuedTransfers != 8 || cfg.LFS.TransferTimeout != 30*time.Minute {
		t.Fatalf("LFS defaults: %+v", cfg.LFS)
	}
}
