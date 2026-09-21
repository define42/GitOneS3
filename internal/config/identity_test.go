package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validIdentityJSON = `{
  "schemaVersion": 1,
  "shardCount": 256,
  "hashAlgorithm": "xxhash64",
  "canonicalization": "lowercase-ascii-v1",
  "bucketPrefix": "gitone-prod-shard",
  "s3Endpoint": "",
  "s3Region": "eu-north-1",
  "maxTopLevelLength": 63,
  "maxComponentLength": 255,
  "maxPathDepth": 32
}`

func TestConfigClusterIdentity(t *testing.T) {
	t.Parallel()

	cfg := testIdentityConfig()
	want := ClusterIdentity{
		SchemaVersion:      ClusterIdentitySchemaVersion,
		ShardCount:         256,
		HashAlgorithm:      RoutingHashAlgorithm,
		Canonicalization:   CanonicalizationVersion,
		BucketPrefix:       "gitone-prod-shard",
		S3Region:           "eu-north-1",
		MaxTopLevelLength:  DefaultMaxTopLevelLength,
		MaxComponentLength: DefaultMaxComponentLength,
		MaxPathDepth:       DefaultMaxPathDepth,
	}
	if got := cfg.ClusterIdentity(); got != want {
		t.Errorf("ClusterIdentity() = %#v, want %#v", got, want)
	}
}

func TestParseClusterIdentity(t *testing.T) {
	t.Parallel()

	got, err := ParseClusterIdentity(strings.NewReader(validIdentityJSON))
	if err != nil {
		t.Fatalf("ParseClusterIdentity() error = %v", err)
	}
	if got.ShardCount != 256 || got.HashAlgorithm != RoutingHashAlgorithm {
		t.Errorf("ParseClusterIdentity() = %#v, want reference identity", got)
	}
}

func TestParseClusterIdentityRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		document   string
		errorMatch string
	}{
		{name: "empty", document: "", errorMatch: "decode cluster identity"},
		{name: "unknown field", document: `{"schemaVersion":1,"shardCount":1,"hashAlgorithm":"xxhash64","canonicalization":"lowercase-ascii-v1","bucketPrefix":"gitone-shard","s3Endpoint":"","s3Region":"us-east-1","maxTopLevelLength":63,"maxComponentLength":255,"maxPathDepth":32,"shards":1}`, errorMatch: "unknown field"},
		{name: "trailing document", document: validIdentityJSON + `{}`, errorMatch: "trailing json"},
		{name: "missing shard count", document: `{"schemaVersion":1,"hashAlgorithm":"xxhash64","canonicalization":"lowercase-ascii-v1","bucketPrefix":"gitone-shard","s3Region":"us-east-1"}`, errorMatch: "shard count"},
		{name: "missing hash", document: `{"schemaVersion":1,"shardCount":1,"canonicalization":"lowercase-ascii-v1","bucketPrefix":"gitone-shard","s3Region":"us-east-1"}`, errorMatch: "hash algorithm"},
		{name: "missing canonicalization", document: `{"schemaVersion":1,"shardCount":1,"hashAlgorithm":"xxhash64","bucketPrefix":"gitone-shard","s3Region":"us-east-1"}`, errorMatch: "canonicalization"},
		{name: "missing bucket prefix", document: `{"schemaVersion":1,"shardCount":1,"hashAlgorithm":"xxhash64","canonicalization":"lowercase-ascii-v1","s3Region":"us-east-1"}`, errorMatch: "bucket prefix"},
		{name: "missing s3 region", document: `{"schemaVersion":1,"shardCount":1,"hashAlgorithm":"xxhash64","canonicalization":"lowercase-ascii-v1","bucketPrefix":"gitone-shard"}`, errorMatch: "s3 region"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseClusterIdentity(strings.NewReader(test.document))
			if err == nil {
				t.Fatal("ParseClusterIdentity() error = nil, want error")
			}
			if !strings.Contains(err.Error(), test.errorMatch) {
				t.Errorf("ParseClusterIdentity() error = %q, want substring %q", err, test.errorMatch)
			}
		})
	}
}

func TestParseClusterIdentityRejectsNilReader(t *testing.T) {
	t.Parallel()

	_, err := ParseClusterIdentity(nil)
	if err == nil {
		t.Fatal("ParseClusterIdentity(nil) error = nil, want error")
	}
}

func TestLoadClusterIdentity(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cluster-identity.json")
	if err := os.WriteFile(path, []byte(validIdentityJSON), 0o600); err != nil {
		t.Fatalf("write identity fixture: %v", err)
	}
	got, err := LoadClusterIdentity(path)
	if err != nil {
		t.Fatalf("LoadClusterIdentity() error = %v", err)
	}
	if got.ShardCount != 256 {
		t.Errorf("LoadClusterIdentity().ShardCount = %d, want 256", got.ShardCount)
	}
}

func TestConfigValidateClusterIdentity(t *testing.T) {
	t.Parallel()

	cfg := testIdentityConfig()
	if err := cfg.ValidateClusterIdentity(cfg.ClusterIdentity()); err != nil {
		t.Errorf("ValidateClusterIdentity() error = %v, want nil", err)
	}
}

func TestConfigValidateClusterIdentityRejectsMismatch(t *testing.T) {
	t.Parallel()

	cfg := testIdentityConfig()
	tests := []struct {
		name       string
		mutate     func(*ClusterIdentity)
		errorMatch string
	}{
		{name: "schema", mutate: func(i *ClusterIdentity) { i.SchemaVersion = 2 }, errorMatch: "schema version mismatch"},
		{name: "shard count", mutate: func(i *ClusterIdentity) { i.ShardCount = 128 }, errorMatch: "shard count mismatch"},
		{name: "hash", mutate: func(i *ClusterIdentity) { i.HashAlgorithm = "other" }, errorMatch: "hash algorithm mismatch"},
		{name: "canonicalization", mutate: func(i *ClusterIdentity) { i.Canonicalization = "other" }, errorMatch: "canonicalization mismatch"},
		{name: "bucket prefix", mutate: func(i *ClusterIdentity) { i.BucketPrefix = "other-shard" }, errorMatch: "bucket prefix mismatch"},
		{name: "s3 endpoint", mutate: func(i *ClusterIdentity) { i.S3Endpoint = "https://other.example" }, errorMatch: "s3 endpoint mismatch"},
		{name: "s3 region", mutate: func(i *ClusterIdentity) { i.S3Region = "us-west-2" }, errorMatch: "s3 region mismatch"},
		{name: "top-level length", mutate: func(i *ClusterIdentity) { i.MaxTopLevelLength = 48 }, errorMatch: "top-level length mismatch"},
		{name: "component length", mutate: func(i *ClusterIdentity) { i.MaxComponentLength = 128 }, errorMatch: "component length mismatch"},
		{name: "path depth", mutate: func(i *ClusterIdentity) { i.MaxPathDepth = 12 }, errorMatch: "path depth mismatch"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			persisted := cfg.ClusterIdentity()
			test.mutate(&persisted)
			err := cfg.ValidateClusterIdentity(persisted)
			if err == nil {
				t.Fatal("ValidateClusterIdentity() error = nil, want mismatch error")
			}
			if !strings.Contains(err.Error(), test.errorMatch) {
				t.Errorf("ValidateClusterIdentity() error = %q, want substring %q", err, test.errorMatch)
			}
		})
	}
}

func testIdentityConfig() Config {
	return Config{
		ShardCount: 256,
		S3: S3{
			BucketPrefix: "gitone-prod-shard",
			Region:       "eu-north-1",
		},
		Path: PathPolicy{
			MaxTopLevelLength:  DefaultMaxTopLevelLength,
			MaxComponentLength: DefaultMaxComponentLength,
			MaxDepth:           DefaultMaxPathDepth,
		},
	}
}
