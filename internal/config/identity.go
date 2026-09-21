package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	ClusterIdentitySchemaVersion = uint32(1)
	RoutingHashAlgorithm         = "xxhash64"
	CanonicalizationVersion      = "lowercase-ascii-v1"
)

// ClusterIdentity is persisted independently of process configuration. A
// mismatch means routing existing namespaces would produce different owners.
type ClusterIdentity struct {
	SchemaVersion      uint32 `json:"schemaVersion"`
	ShardCount         uint32 `json:"shardCount"`
	HashAlgorithm      string `json:"hashAlgorithm"`
	Canonicalization   string `json:"canonicalization"`
	BucketPrefix       string `json:"bucketPrefix"`
	S3Endpoint         string `json:"s3Endpoint"`
	S3Region           string `json:"s3Region"`
	MaxTopLevelLength  int    `json:"maxTopLevelLength"`
	MaxComponentLength int    `json:"maxComponentLength"`
	MaxPathDepth       int    `json:"maxPathDepth"`
}

// ClusterIdentity returns the routing identity represented by this runtime
// configuration.
func (c Config) ClusterIdentity() ClusterIdentity {
	return ClusterIdentity{
		SchemaVersion:      ClusterIdentitySchemaVersion,
		ShardCount:         c.ShardCount,
		HashAlgorithm:      RoutingHashAlgorithm,
		Canonicalization:   CanonicalizationVersion,
		BucketPrefix:       c.S3.BucketPrefix,
		S3Endpoint:         c.S3.Endpoint,
		S3Region:           c.S3.Region,
		MaxTopLevelLength:  c.Path.MaxTopLevelLength,
		MaxComponentLength: c.Path.MaxComponentLength,
		MaxPathDepth:       c.Path.MaxDepth,
	}
}

// ParseClusterIdentity decodes the versioned identity format and rejects
// unknown fields so misspelled invariants cannot be silently ignored.
func ParseClusterIdentity(r io.Reader) (ClusterIdentity, error) {
	if r == nil {
		return ClusterIdentity{}, fmt.Errorf("config: cluster identity reader is nil")
	}
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()

	var identity ClusterIdentity
	if err := decoder.Decode(&identity); err != nil {
		return ClusterIdentity{}, fmt.Errorf("config: decode cluster identity: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ClusterIdentity{}, fmt.Errorf("config: cluster identity contains trailing json")
		}
		return ClusterIdentity{}, fmt.Errorf("config: decode trailing cluster identity data: %w", err)
	}
	if err := identity.validate(); err != nil {
		return ClusterIdentity{}, err
	}

	return identity, nil
}

// LoadClusterIdentity reads a cluster identity from path.
func LoadClusterIdentity(path string) (ClusterIdentity, error) {
	file, err := os.Open(path) //nolint:gosec // The administrator supplies the mounted identity path.
	if err != nil {
		return ClusterIdentity{}, fmt.Errorf("config: open cluster identity: %w", err)
	}

	identity, parseErr := ParseClusterIdentity(file)
	closeErr := file.Close()
	if parseErr != nil {
		if closeErr != nil {
			return ClusterIdentity{}, errors.Join(
				fmt.Errorf("config: parse cluster identity: %w", parseErr),
				fmt.Errorf("config: close cluster identity: %w", closeErr),
			)
		}
		return ClusterIdentity{}, fmt.Errorf("config: parse cluster identity: %w", parseErr)
	}
	if closeErr != nil {
		return ClusterIdentity{}, fmt.Errorf("config: close cluster identity: %w", closeErr)
	}

	return identity, nil
}

// ValidateClusterIdentity fails closed when persisted routing invariants do not
// exactly match the running binary and environment.
func (c Config) ValidateClusterIdentity(persisted ClusterIdentity) error {
	if err := persisted.validate(); err != nil {
		return err
	}
	runtime := c.ClusterIdentity()
	if persisted.SchemaVersion != runtime.SchemaVersion {
		return fmt.Errorf(
			"config: cluster identity schema version mismatch: persisted %d, runtime %d",
			persisted.SchemaVersion,
			runtime.SchemaVersion,
		)
	}
	if persisted.ShardCount != runtime.ShardCount {
		return fmt.Errorf(
			"config: cluster shard count mismatch: persisted %d, runtime %d",
			persisted.ShardCount,
			runtime.ShardCount,
		)
	}
	if persisted.HashAlgorithm != runtime.HashAlgorithm {
		return fmt.Errorf(
			"config: cluster hash algorithm mismatch: persisted %q, runtime %q",
			persisted.HashAlgorithm,
			runtime.HashAlgorithm,
		)
	}
	if persisted.Canonicalization != runtime.Canonicalization {
		return fmt.Errorf(
			"config: cluster canonicalization mismatch: persisted %q, runtime %q",
			persisted.Canonicalization,
			runtime.Canonicalization,
		)
	}
	if persisted.BucketPrefix != runtime.BucketPrefix {
		return fmt.Errorf(
			"config: cluster bucket prefix mismatch: persisted %q, runtime %q",
			persisted.BucketPrefix,
			runtime.BucketPrefix,
		)
	}
	if persisted.S3Endpoint != runtime.S3Endpoint {
		return fmt.Errorf(
			"config: cluster s3 endpoint mismatch: persisted %q, runtime %q",
			persisted.S3Endpoint,
			runtime.S3Endpoint,
		)
	}
	if persisted.S3Region != runtime.S3Region {
		return fmt.Errorf(
			"config: cluster s3 region mismatch: persisted %q, runtime %q",
			persisted.S3Region,
			runtime.S3Region,
		)
	}
	if persisted.MaxTopLevelLength != runtime.MaxTopLevelLength {
		return fmt.Errorf(
			"config: cluster maximum top-level length mismatch: persisted %d, runtime %d",
			persisted.MaxTopLevelLength,
			runtime.MaxTopLevelLength,
		)
	}
	if persisted.MaxComponentLength != runtime.MaxComponentLength {
		return fmt.Errorf(
			"config: cluster maximum component length mismatch: persisted %d, runtime %d",
			persisted.MaxComponentLength,
			runtime.MaxComponentLength,
		)
	}
	if persisted.MaxPathDepth != runtime.MaxPathDepth {
		return fmt.Errorf(
			"config: cluster maximum path depth mismatch: persisted %d, runtime %d",
			persisted.MaxPathDepth,
			runtime.MaxPathDepth,
		)
	}

	return nil
}

func (i ClusterIdentity) validate() error {
	if i.SchemaVersion == 0 {
		return fmt.Errorf("config: cluster identity schema version is required")
	}
	if i.ShardCount == 0 {
		return fmt.Errorf("config: cluster identity shard count must be greater than zero")
	}
	if i.HashAlgorithm == "" {
		return fmt.Errorf("config: cluster identity hash algorithm is required")
	}
	if i.Canonicalization == "" {
		return fmt.Errorf("config: cluster identity canonicalization is required")
	}
	if err := validateBucketPrefix(i.BucketPrefix, i.ShardCount); err != nil {
		return fmt.Errorf("config: cluster identity bucket prefix: %w", err)
	}
	if i.S3Region == "" {
		return fmt.Errorf("config: cluster identity s3 region is required")
	}
	if i.MaxTopLevelLength <= 0 || i.MaxComponentLength <= 0 {
		return fmt.Errorf("config: cluster identity path component limits must be positive")
	}
	if i.MaxTopLevelLength > i.MaxComponentLength {
		return fmt.Errorf("config: cluster identity top-level length exceeds component length")
	}
	if i.MaxPathDepth < 2 {
		return fmt.Errorf("config: cluster identity path depth must be at least two")
	}

	return nil
}
