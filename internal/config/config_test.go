package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	got, err := Load(testLookup(baseEnvironment()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Config{
		ShardCount:          256,
		LocalShard:          173,
		ListenAddress:       DefaultListenAddress,
		PublicPort:          DefaultPublicPort,
		InternalPort:        DefaultInternalPort,
		InternalScheme:      DefaultInternalScheme,
		HeadlessService:     DefaultHeadlessService,
		Namespace:           "gitone-system",
		ClusterIdentityFile: DefaultClusterIdentityFile,
		S3: S3{
			Region:       DefaultS3Region,
			UseTLS:       true,
			BucketPrefix: DefaultS3BucketPrefix,
			Bucket:       "gitone-shard-173",
		},
		Pack: PackPolicy{
			CompactOnPush:               true,
			MaxPackCount:                DefaultMaxPackCount,
			MaxSmallPackCount:           DefaultMaxSmallPackCount,
			SmallPackThreshold:          DefaultSmallPackThreshold,
			LiveCompactionMaxInputBytes: DefaultLiveCompactionInputBytes,
			LiveCompactionMaxInputPacks: DefaultLiveCompactionInputPacks,
		},
		Path: PathPolicy{
			MaxTopLevelLength:  DefaultMaxTopLevelLength,
			MaxComponentLength: DefaultMaxComponentLength,
			MaxDepth:           DefaultMaxPathDepth,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	environment := baseEnvironment()
	environment["GITONE_SHARD_COUNT"] = "11"
	environment["POD_NAME"] = "gitone-10"
	environment["POD_NAMESPACE"] = "source-control"
	environment["GITONE_LISTEN_ADDRESS"] = "127.0.0.1"
	environment["GITONE_PUBLIC_PORT"] = "9000"
	environment["GITONE_INTERNAL_PORT"] = "9001"
	environment["GITONE_HEADLESS_SERVICE"] = "shards"
	environment["GITONE_CLUSTER_IDENTITY_FILE"] = "/run/gitone/identity.json"
	environment["GITONE_S3_ENDPOINT"] = "http://minio.storage.svc:9000"
	environment["GITONE_S3_REGION"] = "eu-north-1"
	environment["GITONE_S3_PATH_STYLE"] = "true"
	environment["GITONE_S3_TLS"] = "false"
	environment["GITONE_S3_BUCKET_PREFIX"] = "gitone-prod-shard"
	environment["GITONE_PACK_COMPACT_ON_PUSH"] = "false"
	environment["GITONE_PACK_MAX_COUNT"] = "64"
	environment["GITONE_PACK_MAX_SMALL_COUNT"] = "8"
	environment["GITONE_PACK_SMALL_THRESHOLD"] = "64MiB"
	environment["GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_BYTES"] = "1GiB"
	environment["GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_PACKS"] = "16"
	environment["GITONE_PATH_MAX_TOP_LEVEL_LENGTH"] = "48"
	environment["GITONE_PATH_MAX_COMPONENT_LENGTH"] = "128"
	environment["GITONE_PATH_MAX_DEPTH"] = "12"

	got, err := Load(testLookup(environment))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ShardCount != 11 || got.LocalShard != 10 {
		t.Errorf("Load() shard identity = (%d, %d), want (11, 10)", got.ShardCount, got.LocalShard)
	}
	if got.S3.Bucket != "gitone-prod-shard-10" {
		t.Errorf("Load() bucket = %q, want %q", got.S3.Bucket, "gitone-prod-shard-10")
	}
	if got.S3.UseTLS || !got.S3.UsePathStyle {
		t.Errorf("Load() s3 transport = %#v, want path-style http", got.S3)
	}
	if got.Path != (PathPolicy{MaxTopLevelLength: 48, MaxComponentLength: 128, MaxDepth: 12}) {
		t.Errorf("Load() path policy = %#v, want overridden policy", got.Path)
	}
	if got.Pack.CompactOnPush || got.Pack.SmallPackThreshold != 64<<20 {
		t.Errorf("Load() pack policy = %#v, want overridden policy", got.Pack)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		key        string
		value      string
		deleteKey  bool
		errorMatch string
	}{
		{name: "missing shard count", key: "GITONE_SHARD_COUNT", deleteKey: true, errorMatch: "is required"},
		{name: "zero shard count", key: "GITONE_SHARD_COUNT", value: "0", errorMatch: "positive uint32"},
		{name: "shard count overflow", key: "GITONE_SHARD_COUNT", value: "4294967296", errorMatch: "positive uint32"},
		{name: "missing pod name", key: "POD_NAME", deleteKey: true, errorMatch: "POD_NAME is required"},
		{name: "malformed pod name", key: "POD_NAME", value: "worker-1", errorMatch: "must match gitone-N"},
		{name: "noncanonical ordinal", key: "POD_NAME", value: "gitone-01", errorMatch: "canonical decimal"},
		{name: "ordinal outside count", key: "POD_NAME", value: "gitone-256", errorMatch: "outside configured"},
		{name: "missing namespace", key: "POD_NAMESPACE", deleteKey: true, errorMatch: "pod namespace"},
		{name: "invalid namespace", key: "POD_NAMESPACE", value: "GitOne", errorMatch: "dns label"},
		{name: "invalid public port", key: "GITONE_PUBLIC_PORT", value: "70000", errorMatch: "port between"},
		{name: "same ports", key: "GITONE_INTERNAL_PORT", value: "8080", errorMatch: "ports must differ"},
		{name: "unsupported internal https", key: "GITONE_INTERNAL_SCHEME", value: "https", errorMatch: "must be http"},
		{name: "invalid internal scheme", key: "GITONE_INTERNAL_SCHEME", value: "tcp", errorMatch: "must be http"},
		{name: "invalid path style", key: "GITONE_S3_PATH_STYLE", value: "sometimes", errorMatch: "must be a boolean"},
		{name: "uppercase bucket prefix", key: "GITONE_S3_BUCKET_PREFIX", value: "GitOne", errorMatch: "dns-safe"},
		{name: "relative identity path", key: "GITONE_CLUSTER_IDENTITY_FILE", value: "identity.json", errorMatch: "absolute path"},
		{name: "bad endpoint", key: "GITONE_S3_ENDPOINT", value: "minio:9000", errorMatch: "absolute http or https"},
		{name: "endpoint path", key: "GITONE_S3_ENDPOINT", value: "https://s3.example/base", errorMatch: "cannot contain a path"},
		{name: "tls endpoint mismatch", key: "GITONE_S3_ENDPOINT", value: "http://minio:9000", errorMatch: "tls requires"},
		{name: "zero pack count", key: "GITONE_PACK_MAX_COUNT", value: "0", errorMatch: "positive uint32"},
		{name: "small count above total", key: "GITONE_PACK_MAX_SMALL_COUNT", value: "33", errorMatch: "cannot exceed"},
		{name: "invalid threshold", key: "GITONE_PACK_SMALL_THRESHOLD", value: "128MB", errorMatch: "positive byte size"},
		{name: "live bytes below threshold", key: "GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_BYTES", value: "64MiB", errorMatch: "cannot be below"},
		{name: "zero live input packs", key: "GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_PACKS", value: "0", errorMatch: "positive uint32"},
		{name: "zero top-level length", key: "GITONE_PATH_MAX_TOP_LEVEL_LENGTH", value: "0", errorMatch: "positive integer"},
		{name: "top-level exceeds component", key: "GITONE_PATH_MAX_TOP_LEVEL_LENGTH", value: "256", errorMatch: "cannot exceed"},
		{name: "path depth too small", key: "GITONE_PATH_MAX_DEPTH", value: "1", errorMatch: "at least two"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := baseEnvironment()
			if test.deleteKey {
				delete(environment, test.key)
			} else {
				environment[test.key] = test.value
			}

			_, err := Load(testLookup(environment))
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			if !strings.Contains(err.Error(), test.errorMatch) {
				t.Errorf("Load() error = %q, want substring %q", err, test.errorMatch)
			}
		})
	}
}

func TestLoadRejectsNilLookup(t *testing.T) {
	t.Parallel()

	_, err := Load(nil)
	if err == nil || !strings.Contains(err.Error(), "lookup is nil") {
		t.Errorf("Load(nil) error = %v, want nil lookup error", err)
	}
}

func TestLoadRejectsDisabledS3TLSWithoutEndpoint(t *testing.T) {
	t.Parallel()

	environment := baseEnvironment()
	environment["GITONE_S3_TLS"] = "false"
	_, err := Load(testLookup(environment))
	if err == nil || !strings.Contains(err.Error(), "requires an explicit http endpoint") {
		t.Errorf("Load() error = %v, want missing insecure endpoint error", err)
	}
}

func TestConfigValidateRejectsInvalidPackPolicy(t *testing.T) {
	t.Parallel()

	cfg, err := Load(testLookup(baseEnvironment()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.Pack.SmallPackThreshold = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid pack policy error")
	}
}

func TestConfigValidateRejectsInconsistentBucket(t *testing.T) {
	t.Parallel()

	cfg, err := Load(testLookup(baseEnvironment()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.S3.Bucket = "another-shard-173"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want derived bucket error")
	}
}

func TestConfigBucketFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shardCount uint32
		shard      uint32
		expected   string
	}{
		{name: "single shard", shardCount: 1, shard: 0, expected: "test-0"},
		{name: "ten shards", shardCount: 10, shard: 9, expected: "test-9"},
		{name: "eleven shards", shardCount: 11, shard: 9, expected: "test-09"},
		{name: "reference deployment", shardCount: 256, shard: 7, expected: "test-007"},
		{name: "four digit ordinal", shardCount: 1001, shard: 42, expected: "test-0042"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{ShardCount: test.shardCount, S3: S3{BucketPrefix: "test"}}
			got, err := cfg.BucketFor(test.shard)
			if err != nil {
				t.Fatalf("BucketFor(%d) error = %v", test.shard, err)
			}
			if got != test.expected {
				t.Errorf("BucketFor(%d) = %q, want %q", test.shard, got, test.expected)
			}
		})
	}
}

func TestConfigBucketForRejectsOutOfRangeShard(t *testing.T) {
	t.Parallel()

	cfg := Config{ShardCount: 4, S3: S3{BucketPrefix: "test"}}
	_, err := cfg.BucketFor(4)
	if err == nil {
		t.Fatal("BucketFor(4) error = nil, want out-of-range error")
	}
}

func TestParseBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		expected  int64
		wantError bool
	}{
		{name: "bytes", input: "128", expected: 128},
		{name: "explicit bytes", input: "128B", expected: 128},
		{name: "mebibytes", input: "128MiB", expected: 128 << 20},
		{name: "gibibytes", input: "2GiB", expected: 2 << 30},
		{name: "empty", input: "", wantError: true},
		{name: "unsupported unit", input: "128MB", wantError: true},
		{name: "overflow", input: "9223372036854775807GiB", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBytes(test.input)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseBytes(%q) error = nil, want error", test.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBytes(%q) error = %v", test.input, err)
			}
			if got != test.expected {
				t.Errorf("parseBytes(%q) = %d, want %d", test.input, got, test.expected)
			}
		})
	}
}

func baseEnvironment() map[string]string {
	return map[string]string{
		"GITONE_SHARD_COUNT": "256",
		"POD_NAME":           "gitone-173",
		"POD_NAMESPACE":      "gitone-system",
	}
}

func testLookup(environment map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	}
}
