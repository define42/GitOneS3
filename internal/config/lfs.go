package config

import (
	"fmt"
	"strconv"
	"time"
)

// LFS bounds streamed large-file transfers through each owning shard process.
type LFS struct {
	Enabled                bool
	MaxObjectBytes         int64
	MaxRepositoryBytes     int64
	MaxConcurrentTransfers int
	MaxQueuedTransfers     int
	QueueTimeout           time.Duration
	TransferTimeout        time.Duration
}

func loadLFS(lookup LookupEnv) (LFS, error) {
	enabled, err := boolValue(lookup, "GITONE_LFS_ENABLED", true)
	if err != nil {
		return LFS{}, err
	}
	object, err := bytesValue(lookup, "GITONE_LFS_MAX_OBJECT_BYTES", 1<<30)
	if err != nil {
		return LFS{}, err
	}
	repository, err := bytesValue(lookup, "GITONE_LFS_MAX_REPOSITORY_BYTES", 10<<30)
	if err != nil {
		return LFS{}, err
	}
	active, err := intValue(lookup, "GITONE_LFS_MAX_CONCURRENT_TRANSFERS", 4)
	if err != nil {
		return LFS{}, err
	}
	queued, err := strconv.Atoi(value(lookup, "GITONE_LFS_MAX_QUEUED_TRANSFERS", "8"))
	if err != nil {
		return LFS{}, fmt.Errorf("config: GITONE_LFS_MAX_QUEUED_TRANSFERS must be an integer")
	}
	queueTimeout, err := time.ParseDuration(value(lookup, "GITONE_LFS_QUEUE_TIMEOUT", "5s"))
	if err != nil || queueTimeout <= 0 {
		return LFS{}, fmt.Errorf("config: GITONE_LFS_QUEUE_TIMEOUT must be a positive duration")
	}
	transferTimeout, err := time.ParseDuration(value(lookup, "GITONE_LFS_TRANSFER_TIMEOUT", "30m"))
	if err != nil || transferTimeout <= 0 {
		return LFS{}, fmt.Errorf("config: GITONE_LFS_TRANSFER_TIMEOUT must be a positive duration")
	}
	return LFS{Enabled: enabled, MaxObjectBytes: object, MaxRepositoryBytes: repository,
		MaxConcurrentTransfers: active, MaxQueuedTransfers: queued, QueueTimeout: queueTimeout, TransferTimeout: transferTimeout}, nil
}

func (c LFS) validate() error {
	if c.MaxObjectBytes < 0 || c.MaxObjectBytes > 64<<30 || c.MaxRepositoryBytes < 0 || c.MaxRepositoryBytes > 1<<50 {
		return fmt.Errorf("config: LFS object limit must not exceed 64GiB and repository quota must not exceed 1PiB")
	}
	object, repository := c.MaxObjectBytes, c.MaxRepositoryBytes
	if object == 0 {
		object = 1 << 30
	}
	if repository == 0 {
		repository = 10 << 30
	}
	if repository < object {
		return fmt.Errorf("config: LFS repository quota must cover one maximum-sized object")
	}
	if c.MaxConcurrentTransfers < 0 || c.MaxConcurrentTransfers > 32 || c.MaxQueuedTransfers < 0 || c.MaxQueuedTransfers > 1024 {
		return fmt.Errorf("config: LFS concurrency must be 1..32 (zero selects default) and queued transfers 0..1024")
	}
	if c.QueueTimeout < 0 || c.QueueTimeout > 90*time.Second || c.TransferTimeout < 0 || c.TransferTimeout > 12*time.Hour {
		return fmt.Errorf("config: LFS queue timeout must not exceed 90s and transfer timeout must not exceed 12h (zero selects defaults)")
	}
	return nil
}
