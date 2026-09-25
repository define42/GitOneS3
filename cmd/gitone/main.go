package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/define42/GitOneS3/internal/app"
	"github.com/define42/GitOneS3/internal/config"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(
		ctx,
		logger,
		os.Args[1:],
		os.LookupEnv,
		os.Stdout,
	); err != nil {
		logger.Error("gitone stopped", "error", err)
		os.Exit(1)
	}
}

const usage = `Usage: gitone [serve | backfill-space-index]

With no command, serve HTTP and optional SSH using environment configuration.

backfill-space-index rebuilds the configured shard's discovery index and exits.
It requires GITONE_SPACE_DISCOVERY_MODE=scan. Before running it, replace ALL old
cluster writers with the index-writing release in scan mode. Run once per shard,
then switch all shards to indexed mode. No OIDC discovery or listeners are used.

Routing and storage use the normal environment, mounted cluster identity, and
AWS credentials. Required: GITONE_SHARD_COUNT, POD_NAME, and POD_NAMESPACE.
GITONE_SPACE_DISCOVERY_MODE is scan or indexed (default indexed).
See docs/space-discovery.md for the rollout and rollback procedure.
`

func run(
	ctx context.Context,
	logger *slog.Logger,
	args []string,
	lookup config.LookupEnv,
	output io.Writer,
) error {
	if len(args) > 1 {
		return fmt.Errorf("invalid command; use gitone --help")
	}
	command := "serve"
	if len(args) == 1 {
		command = args[0]
	}
	switch command {
	case "--help", "-h", "help":
		_, err := io.WriteString(output, usage)
		return err
	case "serve", "backfill-space-index":
	default:
		return fmt.Errorf("invalid command; use gitone --help")
	}
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	if command == "backfill-space-index" {
		if err := app.BackfillSpaceIndex(ctx, cfg); err != nil {
			return err
		}
		logger.Info(
			"space index backfill complete",
			"shard_id", cfg.LocalShard,
			"bucket", cfg.S3.Bucket,
		)
		return nil
	}
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}

	logger.Info(
		"gitone starting",
		"version", version,
		"shard_id", cfg.LocalShard,
		"shard_count", cfg.ShardCount,
		"bucket", cfg.S3.Bucket,
		"space_discovery_mode", cfg.SpaceDiscoveryMode,
	)
	return application.Run(ctx)
}
