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
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/httpserver"
	"github.com/define42/GitOneS3/internal/lfs"
	"github.com/define42/GitOneS3/internal/metrics"
	"github.com/define42/GitOneS3/internal/protocol"
	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/sshserver"
	"github.com/define42/GitOneS3/internal/webui"
)

// App owns the HTTP listener and forwarding transport for one shard.
type App struct {
	server           *httpserver.Server
	sshServer        *sshserver.Server
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

	resources, err := openShard(ctx, cfg)
	if err != nil {
		return nil, err
	}
	objectStore, err := metrics.NewStore(resources.store)
	if err != nil {
		return nil, fmt.Errorf("create storage metrics: %w", err)
	}
	parser, router := resources.parser, resources.router
	if cfg.Auth.Enabled && cfg.SpaceDiscoveryMode != "scan" {
		if err := auth.InitializeSpaceIndex(ctx, objectStore, router, shard.ShardID(cfg.LocalShard)); err != nil {
			return nil, fmt.Errorf("initialize space discovery index: %w", err)
		}
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
	var sshServer *sshserver.Server
	if cfg.Auth.Enabled {
		repositories, err := repository.New(objectStore)
		if err != nil {
			return nil, fmt.Errorf("create repository store: %w", err)
		}
		gitHandler, err := gittransport.New(repositories, gittransport.Options{
			MaxConcurrentOperations: cfg.Git.MaxConcurrentOperations,
			MaxQueuedOperations:     cfg.Git.MaxQueuedOperations,
			QueueTimeout:            cfg.Git.QueueTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("create Git transport: %w", err)
		}
		var lfsHandler http.Handler
		if cfg.LFS.Enabled {
			lfsHandler, err = lfs.New(repositories, lfs.Options{
				PublicURL: cfg.Auth.PublicURL, MaxObjectBytes: cfg.LFS.MaxObjectBytes,
				MaxRepositoryBytes: cfg.LFS.MaxRepositoryBytes, MaxConcurrentTransfers: cfg.LFS.MaxConcurrentTransfers,
				MaxQueuedTransfers: cfg.LFS.MaxQueuedTransfers, QueueTimeout: cfg.LFS.QueueTimeout,
				TransferTimeout: cfg.LFS.TransferTimeout,
			})
			if err != nil {
				return nil, fmt.Errorf("create LFS transport: %w", err)
			}
		}
		ownerHandler = protocol.NewHandler(gitHandler, lfsHandler)
		provider, err := auth.NewOIDC(ctx, cfg.Auth)
		if err != nil {
			return nil, fmt.Errorf("create OIDC authentication: %w", err)
		}
		authHandler, err := auth.New(auth.Options{
			Config: cfg.Auth, LocalShard: shard.ShardID(cfg.LocalShard),
			Router: router, Store: objectStore, Provider: provider, Next: ownerHandler,
			TokenResolver: destinationResolver, TokenTransport: forwardTransport,
			SSHPublicURL: cfg.SSH.PublicURL, SpaceDiscoveryMode: cfg.SpaceDiscoveryMode,
		})
		if err != nil {
			return nil, fmt.Errorf("create authentication handler: %w", err)
		}
		ownerHandler = authHandler
		requestRouter = authHandler
		if cfg.SSH.Enabled {
			hostKey, err := sshserver.LoadSigner(cfg.SSH.HostKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load SSH host key: %w", err)
			}
			forwardKey, err := sshserver.LoadSigner(cfg.SSH.ForwardKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load SSH forwarding key: %w", err)
			}
			sshServer, err = sshserver.New(sshserver.Options{
				Address:    listenAddress(cfg.ListenAddress, cfg.SSH.Port),
				LocalShard: shard.ShardID(cfg.LocalShard), Router: router,
				HostKey: hostKey, ForwardKey: forwardKey, Authority: authHandler, Git: gitHandler, Logger: logger,
				PeerAddress: func(id shard.ShardID) (string, error) {
					destination, err := destinationResolver.Resolve(id)
					if err != nil {
						return "", err
					}
					return listenAddress(destination.Hostname(), cfg.SSH.Port), nil
				},
			})
			if err != nil {
				return nil, fmt.Errorf("create SSH server: %w", err)
			}
		}
	}
	routingHandler, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard:         shard.ShardID(cfg.LocalShard),
		Router:             requestRouter,
		Resolver:           destinationResolver,
		Next:               ownerHandler,
		Transport:          forwardTransport,
		LFSTransferTimeout: cfg.LFS.TransferTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create routing handler: %w", err)
	}
	uiHandler, err := webui.NewHandler(routingHandler, parser)
	if err != nil {
		return nil, fmt.Errorf("create browser interface: %w", err)
	}

	shardLogger := logger.With("shard_id", cfg.LocalShard)
	publicHandler := httpserver.WithHealth(
		httpserver.LogRequests(withMetrics(uiHandler, objectStore, cfg.MetricsToken), shardLogger),
		resources.store,
	)
	server, err := httpserver.New(
		listenAddress(cfg.ListenAddress, cfg.PublicPort),
		publicHandler,
		shardLogger,
	)
	if err != nil {
		return nil, fmt.Errorf("create HTTP server: %w", err)
	}

	return &App{server: server, sshServer: sshServer, forwardTransport: forwardTransport}, nil
}

// Run serves until context cancellation or a listener error.
func (a *App) Run(ctx context.Context) error {
	defer a.forwardTransport.CloseIdleConnections()
	if a.sshServer == nil {
		return a.server.Run(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- a.server.Run(ctx) }()
	go func() { results <- a.sshServer.Run(ctx) }()
	first := <-results
	cancel()
	return errors.Join(first, <-results)
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
