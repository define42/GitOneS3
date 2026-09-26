// Package config loads and validates the process configuration shared by all
// GitOne shard pods.
package config

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddress       = "0.0.0.0"
	DefaultPublicPort          = uint16(8080)
	DefaultInternalScheme      = "http"
	DefaultHeadlessService     = "gitone-headless"
	DefaultS3Region            = "us-east-1"
	DefaultS3BucketPrefix      = "gitone-shard"
	DefaultClusterIdentityFile = "/etc/gitone/identity/cluster-identity.json"
	DefaultMaxTopLevelLength   = 63
	DefaultMaxComponentLength  = 255
	DefaultMaxPathDepth        = 32
	DefaultSpaceDiscoveryMode  = "indexed"

	DefaultGitMaxConcurrentOperations = 1
	DefaultGitMaxQueuedOperations     = 4
	DefaultGitQueueTimeout            = 5 * time.Second

	DefaultMaxPackCount             = uint32(32)
	DefaultMaxSmallPackCount        = uint32(16)
	DefaultSmallPackThreshold       = int64(128 << 20)
	DefaultLiveCompactionInputBytes = int64(2 << 30)
	DefaultLiveCompactionInputPacks = uint32(32)
)

var (
	dnsLabelPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	bucketPrefixPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	podNamePattern      = regexp.MustCompile(`^gitone-([0-9]+)$`)
)

// LookupEnv is compatible with os.LookupEnv and makes environment loading
// deterministic in tests.
type LookupEnv func(string) (string, bool)

// Config is the validated runtime configuration for one shard pod.
type Config struct {
	ShardCount          uint32
	LocalShard          uint32
	ListenAddress       string
	PublicPort          uint16
	InternalScheme      string
	HeadlessService     string
	Namespace           string
	ClusterIdentityFile string
	SpaceDiscoveryMode  string
	MetricsToken        string
	S3                  S3
	Pack                PackPolicy
	Path                PathPolicy
	Auth                Auth
	SSH                 SSH
	Git                 Git
}

// Git bounds active and queued Smart HTTP and SSH work per shard process.
// Programmatic zero active/timeout values select their defaults; zero queued
// operations disables waiting. Load supplies explicit defaults for all fields.
type Git struct {
	MaxConcurrentOperations int
	MaxQueuedOperations     int
	QueueTimeout            time.Duration
}

// S3 configures the shard-local S3-compatible object store. Bucket is derived
// from BucketPrefix, ShardCount, and LocalShard; it is never read from env.
type S3 struct {
	Endpoint     string
	Region       string
	UsePathStyle bool
	UseTLS       bool
	BucketPrefix string
	Bucket       string
}

// PackPolicy controls fragmentation-driven compaction on the push path.
type PackPolicy struct {
	CompactOnPush               bool
	MaxPackCount                uint32
	MaxSmallPackCount           uint32
	SmallPackThreshold          int64
	LiveCompactionMaxInputBytes int64
	LiveCompactionMaxInputPacks uint32
}

// PathPolicy bounds namespace and repository path processing before routing.
type PathPolicy struct {
	MaxTopLevelLength  int
	MaxComponentLength int
	MaxDepth           int
}

// Load reads configuration through lookup, applies architecture defaults, and
// rejects configurations that could assign a pod to the wrong shard or bucket.
func Load(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("config: environment lookup is nil")
	}

	shardCount, err := requiredUint32(lookup, "GITONE_SHARD_COUNT")
	if err != nil {
		return Config{}, err
	}

	localShard, err := localShardFromPodName(requiredValue(lookup, "POD_NAME"))
	if err != nil {
		return Config{}, err
	}

	publicPort, err := uint16Value(lookup, "GITONE_PUBLIC_PORT", DefaultPublicPort)
	if err != nil {
		return Config{}, err
	}
	usePathStyle, err := boolValue(lookup, "GITONE_S3_PATH_STYLE", false)
	if err != nil {
		return Config{}, err
	}
	useTLS, err := boolValue(lookup, "GITONE_S3_TLS", true)
	if err != nil {
		return Config{}, err
	}
	compactOnPush, err := boolValue(lookup, "GITONE_PACK_COMPACT_ON_PUSH", true)
	if err != nil {
		return Config{}, err
	}
	maxPackCount, err := uint32Value(lookup, "GITONE_PACK_MAX_COUNT", DefaultMaxPackCount)
	if err != nil {
		return Config{}, err
	}
	maxSmallPackCount, err := uint32Value(
		lookup,
		"GITONE_PACK_MAX_SMALL_COUNT",
		DefaultMaxSmallPackCount,
	)
	if err != nil {
		return Config{}, err
	}
	smallPackThreshold, err := bytesValue(
		lookup,
		"GITONE_PACK_SMALL_THRESHOLD",
		DefaultSmallPackThreshold,
	)
	if err != nil {
		return Config{}, err
	}
	maxInputBytes, err := bytesValue(
		lookup,
		"GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_BYTES",
		DefaultLiveCompactionInputBytes,
	)
	if err != nil {
		return Config{}, err
	}
	maxInputPacks, err := uint32Value(
		lookup,
		"GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_PACKS",
		DefaultLiveCompactionInputPacks,
	)
	if err != nil {
		return Config{}, err
	}
	maxTopLevelLength, err := intValue(
		lookup,
		"GITONE_PATH_MAX_TOP_LEVEL_LENGTH",
		DefaultMaxTopLevelLength,
	)
	if err != nil {
		return Config{}, err
	}
	maxComponentLength, err := intValue(
		lookup,
		"GITONE_PATH_MAX_COMPONENT_LENGTH",
		DefaultMaxComponentLength,
	)
	if err != nil {
		return Config{}, err
	}
	maxPathDepth, err := intValue(
		lookup,
		"GITONE_PATH_MAX_DEPTH",
		DefaultMaxPathDepth,
	)
	if err != nil {
		return Config{}, err
	}

	auth, err := loadAuth(lookup)
	if err != nil {
		return Config{}, err
	}
	ssh, err := loadSSH(lookup)
	if err != nil {
		return Config{}, err
	}
	git, err := loadGit(lookup)
	if err != nil {
		return Config{}, err
	}
	metricsToken, _ := lookup("GITONE_METRICS_TOKEN")
	cfg := Config{
		Auth:                auth,
		SSH:                 ssh,
		Git:                 git,
		ShardCount:          shardCount,
		LocalShard:          localShard,
		ListenAddress:       value(lookup, "GITONE_LISTEN_ADDRESS", DefaultListenAddress),
		PublicPort:          publicPort,
		InternalScheme:      value(lookup, "GITONE_INTERNAL_SCHEME", DefaultInternalScheme),
		HeadlessService:     value(lookup, "GITONE_HEADLESS_SERVICE", DefaultHeadlessService),
		Namespace:           requiredValue(lookup, "POD_NAMESPACE"),
		ClusterIdentityFile: value(lookup, "GITONE_CLUSTER_IDENTITY_FILE", DefaultClusterIdentityFile),
		SpaceDiscoveryMode:  value(lookup, "GITONE_SPACE_DISCOVERY_MODE", DefaultSpaceDiscoveryMode),
		MetricsToken:        metricsToken,
		S3: S3{
			Endpoint:     value(lookup, "GITONE_S3_ENDPOINT", ""),
			Region:       value(lookup, "GITONE_S3_REGION", DefaultS3Region),
			UsePathStyle: usePathStyle,
			UseTLS:       useTLS,
			BucketPrefix: value(lookup, "GITONE_S3_BUCKET_PREFIX", DefaultS3BucketPrefix),
		},
		Pack: PackPolicy{
			CompactOnPush:               compactOnPush,
			MaxPackCount:                maxPackCount,
			MaxSmallPackCount:           maxSmallPackCount,
			SmallPackThreshold:          smallPackThreshold,
			LiveCompactionMaxInputBytes: maxInputBytes,
			LiveCompactionMaxInputPacks: maxInputPacks,
		},
		Path: PathPolicy{
			MaxTopLevelLength:  maxTopLevelLength,
			MaxComponentLength: maxComponentLength,
			MaxDepth:           maxPathDepth,
		},
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	bucket, err := cfg.BucketFor(cfg.LocalShard)
	if err != nil {
		return Config{}, err
	}
	cfg.S3.Bucket = bucket

	return cfg, nil
}

func loadGit(lookup LookupEnv) (Git, error) {
	active, err := intValue(lookup, "GITONE_GIT_MAX_CONCURRENT_OPERATIONS", DefaultGitMaxConcurrentOperations)
	if err != nil {
		return Git{}, err
	}
	queuedInput := value(lookup, "GITONE_GIT_MAX_QUEUED_OPERATIONS", strconv.Itoa(DefaultGitMaxQueuedOperations))
	queued, err := strconv.Atoi(queuedInput)
	if err != nil {
		return Git{}, fmt.Errorf("config: GITONE_GIT_MAX_QUEUED_OPERATIONS must be an integer between 0 and 1024")
	}
	timeout, err := time.ParseDuration(value(lookup, "GITONE_GIT_QUEUE_TIMEOUT", DefaultGitQueueTimeout.String()))
	if err != nil || timeout <= 0 {
		return Git{}, fmt.Errorf("config: GITONE_GIT_QUEUE_TIMEOUT must be a positive duration no greater than 90s")
	}
	return Git{MaxConcurrentOperations: active, MaxQueuedOperations: queued, QueueTimeout: timeout}, nil
}

// Validate checks invariants that must hold before the process accepts traffic.
func (c Config) Validate() error {
	if err := validateMetricsToken(c.MetricsToken); err != nil {
		return err
	}
	if c.Git.MaxConcurrentOperations < 0 || c.Git.MaxConcurrentOperations > 32 {
		return fmt.Errorf("config: git maximum concurrent operations must be between 1 and 32 (zero selects the default)")
	}
	if c.Git.MaxQueuedOperations < 0 || c.Git.MaxQueuedOperations > 1024 {
		return fmt.Errorf("config: git maximum queued operations must be between 0 and 1024")
	}
	if c.Git.QueueTimeout < 0 || c.Git.QueueTimeout > 90*time.Second {
		return fmt.Errorf("config: git queue timeout must be positive and no greater than 90s (zero selects the default)")
	}
	switch c.SpaceDiscoveryMode {
	case "", "scan", "indexed":
	default:
		return fmt.Errorf("config: space discovery mode must be scan or indexed")
	}
	if err := c.SSH.validate(c.Auth.Enabled, c.PublicPort); err != nil {
		return err
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	if c.ShardCount == 0 {
		return fmt.Errorf("config: shard count must be greater than zero")
	}
	if c.LocalShard >= c.ShardCount {
		return fmt.Errorf(
			"config: local shard %d is outside configured shard count %d",
			c.LocalShard,
			c.ShardCount,
		)
	}
	if strings.TrimSpace(c.ListenAddress) == "" {
		return fmt.Errorf("config: listen address is required")
	}
	if c.PublicPort == 0 {
		return fmt.Errorf("config: public port must be greater than zero")
	}
	if c.InternalScheme != "http" {
		return fmt.Errorf(
			"config: internal scheme must be http; use transparent transport security for mTLS",
		)
	}
	if err := validateDNSLabel("headless service", c.HeadlessService); err != nil {
		return err
	}
	if err := validateDNSLabel("pod namespace", c.Namespace); err != nil {
		return err
	}
	if !filepath.IsAbs(c.ClusterIdentityFile) {
		return fmt.Errorf("config: cluster identity file must be an absolute path")
	}
	if strings.TrimSpace(c.S3.Region) == "" {
		return fmt.Errorf("config: s3 region is required")
	}
	if err := validateEndpoint(c.S3.Endpoint, c.S3.UseTLS); err != nil {
		return err
	}
	if err := validateBucketPrefix(c.S3.BucketPrefix, c.ShardCount); err != nil {
		return err
	}
	expectedBucket := fmt.Sprintf(
		"%s-%0*d",
		c.S3.BucketPrefix,
		shardWidth(c.ShardCount),
		c.LocalShard,
	)
	if c.S3.Bucket != "" && c.S3.Bucket != expectedBucket {
		return fmt.Errorf("config: local s3 bucket does not match derived shard bucket")
	}
	if c.Pack.MaxPackCount == 0 {
		return fmt.Errorf("config: maximum pack count must be greater than zero")
	}
	if c.Pack.MaxSmallPackCount == 0 {
		return fmt.Errorf("config: maximum small pack count must be greater than zero")
	}
	if c.Pack.MaxSmallPackCount > c.Pack.MaxPackCount {
		return fmt.Errorf("config: maximum small pack count cannot exceed maximum pack count")
	}
	if c.Pack.SmallPackThreshold <= 0 {
		return fmt.Errorf("config: small pack threshold must be greater than zero")
	}
	if c.Pack.LiveCompactionMaxInputBytes < c.Pack.SmallPackThreshold {
		return fmt.Errorf("config: live compaction byte limit cannot be below small pack threshold")
	}
	if c.Pack.LiveCompactionMaxInputPacks == 0 {
		return fmt.Errorf("config: live compaction pack limit must be greater than zero")
	}
	if c.Path.MaxTopLevelLength <= 0 || c.Path.MaxComponentLength <= 0 {
		return fmt.Errorf("config: path component limits must be greater than zero")
	}
	if c.Path.MaxTopLevelLength > c.Path.MaxComponentLength {
		return fmt.Errorf("config: top-level length cannot exceed component length")
	}
	if c.Path.MaxDepth < 2 {
		return fmt.Errorf("config: path depth must be at least two")
	}

	return nil
}

// BucketFor returns the deterministic bucket name for a shard in this cluster.
func (c Config) BucketFor(shard uint32) (string, error) {
	if c.ShardCount == 0 {
		return "", fmt.Errorf("config: shard count must be greater than zero")
	}
	if shard >= c.ShardCount {
		return "", fmt.Errorf("config: shard %d is outside configured shard count %d", shard, c.ShardCount)
	}
	if err := validateBucketPrefix(c.S3.BucketPrefix, c.ShardCount); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%0*d", c.S3.BucketPrefix, shardWidth(c.ShardCount), shard), nil
}

func localShardFromPodName(podName string) (uint32, error) {
	if podName == "" {
		return 0, fmt.Errorf("config: POD_NAME is required")
	}
	matches := podNamePattern.FindStringSubmatch(podName)
	if matches == nil {
		return 0, fmt.Errorf("config: POD_NAME must match gitone-N")
	}
	if len(matches[1]) > 1 && matches[1][0] == '0' {
		return 0, fmt.Errorf("config: POD_NAME ordinal must use canonical decimal notation")
	}
	ordinal, err := strconv.ParseUint(matches[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("config: parse POD_NAME ordinal: %w", err)
	}

	return uint32(ordinal), nil
}

func validateDNSLabel(name, input string) error {
	if len(input) == 0 || len(input) > 63 || !dnsLabelPattern.MatchString(input) {
		return fmt.Errorf("config: %s must be a lowercase dns label", name)
	}

	return nil
}

func validateBucketPrefix(prefix string, shardCount uint32) error {
	if !bucketPrefixPattern.MatchString(prefix) {
		return fmt.Errorf("config: s3 bucket prefix must be a lowercase dns-safe label")
	}
	length := len(prefix) + 1 + shardWidth(shardCount)
	if length < 3 || length > 63 {
		return fmt.Errorf("config: derived s3 bucket name must contain 3 to 63 characters")
	}

	return nil
}

func validateEndpoint(endpoint string, useTLS bool) error {
	if endpoint == "" {
		if !useTLS {
			return fmt.Errorf("config: disabling s3 tls requires an explicit http endpoint")
		}
		return nil
	}
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("config: s3 endpoint must be an absolute http or https url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("config: s3 endpoint must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("config: s3 endpoint cannot contain credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("config: s3 endpoint cannot contain a path")
	}
	if useTLS && parsed.Scheme != "https" {
		return fmt.Errorf("config: s3 tls requires an https endpoint")
	}
	if !useTLS && parsed.Scheme != "http" {
		return fmt.Errorf("config: disabled s3 tls requires an http endpoint")
	}

	return nil
}

func shardWidth(shardCount uint32) int {
	if shardCount <= 1 {
		return 1
	}

	return len(strconv.FormatUint(uint64(shardCount-1), 10))
}

func value(lookup LookupEnv, key, defaultValue string) string {
	input, ok := lookup(key)
	if !ok {
		return defaultValue
	}

	return strings.TrimSpace(input)
}

func requiredValue(lookup LookupEnv, key string) string {
	return value(lookup, key, "")
}

func requiredUint32(lookup LookupEnv, key string) (uint32, error) {
	input := requiredValue(lookup, key)
	if input == "" {
		return 0, fmt.Errorf("config: %s is required", key)
	}
	parsed, err := strconv.ParseUint(input, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("config: %s must be a positive uint32", key)
	}

	return uint32(parsed), nil
}

func uint32Value(lookup LookupEnv, key string, defaultValue uint32) (uint32, error) {
	input, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(input), 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("config: %s must be a positive uint32", key)
	}

	return uint32(parsed), nil
}

func uint16Value(lookup LookupEnv, key string, defaultValue uint16) (uint16, error) {
	input, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(input), 10, 16)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("config: %s must be a port between 1 and 65535", key)
	}

	return uint16(parsed), nil
}

func intValue(lookup LookupEnv, key string, defaultValue int) (int, error) {
	input, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(input), 10, 31)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("config: %s must be a positive integer", key)
	}

	return int(parsed), nil
}

func boolValue(lookup LookupEnv, key string, defaultValue bool) (bool, error) {
	input, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(input))
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean: %w", key, err)
	}

	return parsed, nil
}

func bytesValue(lookup LookupEnv, key string, defaultValue int64) (int64, error) {
	input, ok := lookup(key)
	if !ok {
		return defaultValue, nil
	}
	parsed, err := parseBytes(strings.TrimSpace(input))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive byte size", key)
	}

	return parsed, nil
}

func parseBytes(input string) (int64, error) {
	type unit struct {
		suffix     string
		multiplier uint64
	}
	units := []unit{
		{suffix: "TiB", multiplier: 1 << 40},
		{suffix: "GiB", multiplier: 1 << 30},
		{suffix: "MiB", multiplier: 1 << 20},
		{suffix: "KiB", multiplier: 1 << 10},
		{suffix: "B", multiplier: 1},
	}

	if input == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	multiplier := uint64(1)
	number := input
	for _, candidate := range units {
		if strings.HasSuffix(input, candidate.suffix) {
			multiplier = candidate.multiplier
			number = strings.TrimSpace(strings.TrimSuffix(input, candidate.suffix))
			break
		}
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse byte size: %w", err)
	}
	if parsed > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("byte size overflows int64")
	}

	// #nosec G115 -- The bound above ensures the product fits int64 before conversion.
	return int64(parsed * multiplier), nil
}
