// Package app composes one GitOne shard process from explicit dependencies.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/httpserver"
	"github.com/define42/GitOneS3/internal/protocol"
	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage/s3store"
)

// App owns the HTTP listener and forwarding transport for one shard.
type App struct {
	server           *httpserver.Server
	forwardTransport *http.Transport
}

// New validates immutable cluster identity, creates the fixed-bucket S3
// adapter, and wires public and internal routing.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	identity, err := config.LoadClusterIdentity(cfg.ClusterIdentityFile)
	if err != nil {
		return nil, fmt.Errorf("load immutable cluster identity: %w", err)
	}
	if err := cfg.ValidateClusterIdentity(identity); err != nil {
		return nil, fmt.Errorf("validate immutable cluster identity: %w", err)
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(cfg.S3.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	s3Client := s3.NewFromConfig(awsConfig, configureS3Client(cfg.S3))
	objectStore, err := s3store.New(s3Client, cfg.S3.Bucket)
	if err != nil {
		return nil, fmt.Errorf("create shard object store: %w", err)
	}
	if err := objectStore.Check(ctx); err != nil {
		return nil, fmt.Errorf("check shard object store: %w", err)
	}

	pathPolicy := shard.DefaultPathPolicy()
	pathPolicy.MaxTopLevelLength = cfg.Path.MaxTopLevelLength
	pathPolicy.MaxComponentLength = cfg.Path.MaxComponentLength
	pathPolicy.MaxPathDepth = cfg.Path.MaxDepth
	parser, err := shard.NewParser(pathPolicy)
	if err != nil {
		return nil, fmt.Errorf("create canonical path parser: %w", err)
	}
	router, err := shard.NewRouter(cfg.ShardCount, parser)
	if err != nil {
		return nil, fmt.Errorf("create shard router: %w", err)
	}
	destinationResolver, err := proxy.NewStatefulSetResolver(proxy.StatefulSetResolverOptions{
		Scheme:          cfg.InternalScheme,
		StatefulSet:     "gitone",
		HeadlessService: cfg.HeadlessService,
		Namespace:       cfg.Namespace,
		Port:            cfg.PublicPort,
	})
	if err != nil {
		return nil, fmt.Errorf("create shard destination resolver: %w", err)
	}

	forwardTransport, err := newForwardTransport(cfg.ShardCount)
	if err != nil {
		return nil, err
	}
	var ownerHandler http.Handler = protocol.NewHandler(nil, nil)
	var requestRouter proxy.Router = router
	if cfg.Auth.Enabled {
		provider, err := auth.NewOIDC(ctx, cfg.Auth)
		if err != nil {
			return nil, fmt.Errorf("create OIDC authentication: %w", err)
		}
		authHandler, err := auth.New(auth.Options{
			Config: cfg.Auth, LocalShard: shard.ShardID(cfg.LocalShard),
			Router: router, Store: objectStore, Provider: provider, Next: ownerHandler,
		})
		if err != nil {
			return nil, fmt.Errorf("create authentication handler: %w", err)
		}
		ownerHandler = authHandler
		requestRouter = authHandler
	}
	routingHandler, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard: shard.ShardID(cfg.LocalShard),
		Router:     requestRouter,
		Resolver:   destinationResolver,
		Next:       ownerHandler,
		Transport:  forwardTransport,
	})
	if err != nil {
		return nil, fmt.Errorf("create routing handler: %w", err)
	}

	shardLogger := logger.With("shard_id", cfg.LocalShard)
	publicHandler := httpserver.WithHealth(
		httpserver.LogRequests(routingHandler, shardLogger),
		objectStore,
	)
	server, err := httpserver.New(
		listenAddress(cfg.ListenAddress, cfg.PublicPort),
		publicHandler,
		shardLogger,
	)
	if err != nil {
		return nil, fmt.Errorf("create HTTP server: %w", err)
	}

	return &App{server: server, forwardTransport: forwardTransport}, nil
}

// Run serves until context cancellation or a listener error.
func (a *App) Run(ctx context.Context) error {
	defer a.forwardTransport.CloseIdleConnections()
	return a.server.Run(ctx)
}

func listenAddress(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
}

func configureS3Client(s3Config config.S3) func(*s3.Options) {
	return func(options *s3.Options) {
		options.UsePathStyle = s3Config.UsePathStyle
		// Service-specific environment/shared-config endpoints must not bypass
		// the endpoint pinned in the immutable cluster identity.
		options.BaseEndpoint = nil
		if s3Config.Endpoint != "" {
			options.BaseEndpoint = aws.String(s3Config.Endpoint)
		}
	}
}

func newForwardTransport(shardCount uint32) (*http.Transport, error) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unsupported type")
	}
	transport := defaultTransport.Clone()
	// Forward directly to shard pods without using HTTP_PROXY.
	transport.Proxy = nil
	connectionTarget := min(uint64(shardCount)*4, 65536)
	transport.MaxIdleConns = max(1024, int(connectionTarget))
	transport.MaxIdleConnsPerHost = 64
	return transport, nil
}
