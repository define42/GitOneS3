package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/define42/GitOneS3/internal/app"
	"github.com/define42/GitOneS3/internal/config"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger); err != nil {
		logger.Error("gitone stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
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
	)
	return application.Run(ctx)
}
