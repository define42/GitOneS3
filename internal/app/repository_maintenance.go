package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

var (
	maintenanceSnapshotPattern = regexp.MustCompile(`^states/[0-9]{20}-state-[a-f0-9]{64}\.json$`)
	maintenanceTokenPattern    = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// RepositoryMaintenanceOptions selects one operator action on one repository.
// These commands use direct bucket credentials, not end-user authorization.
type RepositoryMaintenanceOptions struct {
	Operation   string
	Namespace   string
	Repository  string
	Snapshot    string
	Apply       bool
	GracePeriod time.Duration
	Token       string
	Offline     bool
}

// Validate rejects malformed targets and unsafe or irrelevant options before
// configuration loading or any access to the object store.
func (o RepositoryMaintenanceOptions) Validate() error {
	switch o.Operation {
	case "check", "generations", "restore", "gc", "repack", "lock", "unlock":
	default:
		return fmt.Errorf("unknown repository operation %q", o.Operation)
	}
	if o.Namespace == "auth" || strings.Contains(o.Namespace, "/") || !repository.ValidName(o.Repository) {
		return fmt.Errorf("target must be a canonical namespace/repository without a .git suffix")
	}
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		return err
	}
	router, err := shard.NewRouter(1, parser)
	if err != nil {
		return err
	}
	if _, err := router.Owner(o.Namespace); err != nil {
		return fmt.Errorf("invalid repository namespace: %w", err)
	}
	if o.Operation == "restore" {
		if !maintenanceSnapshotPattern.MatchString(o.Snapshot) {
			return fmt.Errorf("restore requires --snapshot with an exact state snapshot key from generations")
		}
	} else if o.Snapshot != "" {
		return fmt.Errorf("--snapshot is only valid for restore")
	}
	if o.Operation == "gc" {
		if o.GracePeriod < 0 {
			return fmt.Errorf("--grace-period must be positive (zero selects the 24h default)")
		}
	} else if o.Apply || o.GracePeriod != 0 {
		return fmt.Errorf("--apply and --grace-period are only valid for gc")
	}
	if o.Operation == "unlock" {
		if !o.Offline || !maintenanceTokenPattern.MatchString(o.Token) {
			return fmt.Errorf("unlock requires --offline and the exact --token reported by lock; stop all repository writers first")
		}
	} else if o.Token != "" || o.Offline {
		return fmt.Errorf("--token and --offline are only valid for unlock")
	}
	return nil
}

// RepositoryMaintenanceResult is the JSON command output, including partial
// progress when a maintenance operation fails. Error is also returned as an
// error so command-line callers can signal failure to automation.
type RepositoryMaintenanceResult struct {
	Operation  string `json:"operation"`
	Namespace  string `json:"namespace"`
	Repository string `json:"repository"`
	Report     any    `json:"report,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MaintainRepository runs without opening listeners or discovering an OIDC
// provider. It uses the normal shard configuration, immutable cluster identity,
// and conditional-operation capability checks before accessing repository data.
func MaintainRepository(
	ctx context.Context,
	cfg config.Config,
	options RepositoryMaintenanceOptions,
) (RepositoryMaintenanceResult, error) {
	return maintainRepository(ctx, cfg, options, func(ctx context.Context, cfg config.Config) (storage.ObjectStore, error) {
		resources, err := openShard(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return resources.store, nil
	})
}

func maintainRepository(
	ctx context.Context,
	cfg config.Config,
	options RepositoryMaintenanceOptions,
	open func(context.Context, config.Config) (storage.ObjectStore, error),
) (result RepositoryMaintenanceResult, err error) {
	result = RepositoryMaintenanceResult{
		Operation: options.Operation, Namespace: options.Namespace, Repository: options.Repository,
	}
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	if err := options.Validate(); err != nil {
		return result, err
	}
	if err := cfg.Validate(); err != nil {
		return result, err
	}
	if err := validateMaintenanceOwner(cfg, options.Namespace); err != nil {
		return result, err
	}
	objects, err := open(ctx, cfg)
	if err != nil {
		return result, err
	}
	repositories, err := repository.New(objects)
	if err != nil {
		return result, fmt.Errorf("create maintenance repository store: %w", err)
	}
	namespace, name := options.Namespace, options.Repository
	switch options.Operation {
	case "check":
		result.Report, err = repositories.CheckIntegrity(ctx, namespace, name)
	case "generations":
		result.Report, err = repositories.ListGenerations(ctx, namespace, name)
	case "restore":
		result.Report, err = repositories.RestoreGeneration(ctx, namespace, name, options.Snapshot)
	case "gc":
		result.Report, err = repositories.GarbageCollect(ctx, namespace, name, repository.GCOptions{
			Apply: options.Apply, GracePeriod: options.GracePeriod,
		})
	case "repack":
		err = repositories.Repack(ctx, namespace, name)
		if err == nil {
			result.Report = struct {
				Repacked bool `json:"repacked"`
			}{true}
		}
	case "lock":
		result.Report, err = repositories.GetMaintenanceLock(ctx, namespace, name)
	case "unlock":
		err = repositories.UnlockMaintenance(ctx, namespace, name, options.Token, options.Offline)
		if err == nil {
			result.Report = struct {
				Unlocked bool `json:"unlocked"`
			}{true}
		}
	}
	return result, err
}

func validateMaintenanceOwner(cfg config.Config, namespace string) error {
	policy := shard.DefaultPathPolicy()
	policy.MaxTopLevelLength = cfg.Path.MaxTopLevelLength
	policy.MaxComponentLength = cfg.Path.MaxComponentLength
	policy.MaxPathDepth = cfg.Path.MaxDepth
	parser, err := shard.NewParser(policy)
	if err != nil {
		return fmt.Errorf("create maintenance namespace parser: %w", err)
	}
	router, err := shard.NewRouter(cfg.ShardCount, parser)
	if err != nil {
		return fmt.Errorf("create maintenance router: %w", err)
	}
	owner, err := router.Owner(namespace)
	if err != nil {
		return fmt.Errorf("validate maintenance namespace: %w", err)
	}
	if owner != shard.ShardID(cfg.LocalShard) {
		return fmt.Errorf("namespace %q belongs to shard %d; run this command on gitone-%d instead of gitone-%d",
			namespace, owner, owner, cfg.LocalShard)
	}
	return nil
}
