package app

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage/s3store"
)

// BackfillSpaceIndex rebuilds discovery candidates for this configured shard
// without starting listeners or contacting the OIDC provider. All cluster
// writers must already run an index-writing release in scan mode. The command
// cannot verify that cluster-wide rollout barrier; the operator must do so.
func BackfillSpaceIndex(ctx context.Context, cfg config.Config) error {
	if cfg.SpaceDiscoveryMode != "scan" {
		return fmt.Errorf("backfill space index requires GITONE_SPACE_DISCOVERY_MODE=scan; " +
			"replace all old cluster writers first")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	resources, err := openShard(ctx, cfg)
	if err != nil {
		return err
	}
	if err := auth.BackfillSpaceIndex(ctx, resources.store, resources.router, shard.ShardID(cfg.LocalShard)); err != nil {
		return fmt.Errorf("backfill shard %d space index: %w", cfg.LocalShard, err)
	}
	return nil
}

type shardResources struct {
	store  *s3store.Store
	parser *shard.Parser
	router *shard.Router
}

// openShard keeps migration and serving bound to the same immutable routing
// identity, fixed bucket, and storage capability checks.
func openShard(ctx context.Context, cfg config.Config) (shardResources, error) {
	identity, err := config.LoadClusterIdentity(cfg.ClusterIdentityFile)
	if err != nil {
		return shardResources{}, fmt.Errorf("load immutable cluster identity: %w", err)
	}
	if err := cfg.ValidateClusterIdentity(identity); err != nil {
		return shardResources{}, fmt.Errorf("validate immutable cluster identity: %w", err)
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.S3.Region))
	if err != nil {
		return shardResources{}, fmt.Errorf("load AWS configuration: %w", err)
	}
	bucket, err := cfg.BucketFor(cfg.LocalShard)
	if err != nil {
		return shardResources{}, err
	}
	client := s3.NewFromConfig(awsConfig, configureS3Client(cfg.S3))
	objectStore, err := s3store.New(client, bucket)
	if err != nil {
		return shardResources{}, fmt.Errorf("create shard object store: %w", err)
	}
	if err := objectStore.Check(ctx); err != nil {
		return shardResources{}, fmt.Errorf("check shard object store: %w", err)
	}
	policy := shard.DefaultPathPolicy()
	policy.MaxTopLevelLength = cfg.Path.MaxTopLevelLength
	policy.MaxComponentLength = cfg.Path.MaxComponentLength
	policy.MaxPathDepth = cfg.Path.MaxDepth
	parser, err := shard.NewParser(policy)
	if err != nil {
		return shardResources{}, fmt.Errorf("create canonical path parser: %w", err)
	}
	router, err := shard.NewRouter(cfg.ShardCount, parser)
	if err != nil {
		return shardResources{}, fmt.Errorf("create shard router: %w", err)
	}
	return shardResources{store: objectStore, parser: parser, router: router}, nil
}
