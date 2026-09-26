package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestMaintainRepositoryRejectsInputBeforeOpeningStorage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options RepositoryMaintenanceOptions
	}{
		{name: "unknown operation", options: RepositoryMaintenanceOptions{Operation: "wipe", Namespace: "alice", Repository: "demo"}},
		{name: "invalid namespace", options: RepositoryMaintenanceOptions{Operation: "check", Namespace: "Alice", Repository: "demo"}},
		{name: "invalid name", options: RepositoryMaintenanceOptions{Operation: "check", Namespace: "alice", Repository: "../demo"}},
		{name: "missing snapshot", options: RepositoryMaintenanceOptions{Operation: "restore", Namespace: "alice", Repository: "demo"}},
		{name: "online unlock", options: RepositoryMaintenanceOptions{Operation: "unlock", Namespace: "alice", Repository: "demo", Token: strings.Repeat("a", 32)}},
		{name: "irrelevant snapshot", options: RepositoryMaintenanceOptions{Operation: "check", Namespace: "alice", Repository: "demo", Snapshot: "snapshot"}},
		{name: "irrelevant apply", options: RepositoryMaintenanceOptions{Operation: "check", Namespace: "alice", Repository: "demo", Apply: true}},
		{name: "irrelevant offline", options: RepositoryMaintenanceOptions{Operation: "check", Namespace: "alice", Repository: "demo", Offline: true}},
		{name: "negative grace", options: RepositoryMaintenanceOptions{Operation: "gc", Namespace: "alice", Repository: "demo", GracePeriod: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := maintainRepository(t.Context(), config.Config{}, test.options, func(context.Context, config.Config) (storage.ObjectStore, error) {
				t.Fatal("opened storage for invalid input")
				return nil, nil
			})
			if err == nil || result.Error == "" || strings.HasPrefix(err.Error(), "config:") {
				t.Fatalf("result = %+v, error = %v; want input validation before config", result, err)
			}
		})
	}
}

func TestMaintainRepositoryRejectsWrongShardBeforeOpeningStorage(t *testing.T) {
	t.Parallel()
	const shards = uint32(4)
	owner := uint32(shard.Sum64([]byte("alice")) % uint64(shards))
	cfg := maintenanceConfig(t, shards, (owner+1)%shards)
	options := RepositoryMaintenanceOptions{Operation: "check", Namespace: "alice", Repository: "demo"}
	result, err := maintainRepository(t.Context(), cfg, options, func(context.Context, config.Config) (storage.ObjectStore, error) {
		t.Fatal("opened storage on the wrong shard")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("belongs to shard %d", owner)) || result.Error == "" {
		t.Fatalf("wrong shard result = %+v, error = %v", result, err)
	}
}

func TestMaintainRepositoryUsesConfiguredNamespacePolicy(t *testing.T) {
	t.Parallel()
	cfg := maintenanceConfig(t, 1, 0)
	cfg.Path.MaxTopLevelLength = 6
	options := RepositoryMaintenanceOptions{Operation: "check", Namespace: "longnamespace", Repository: "demo"}
	if _, err := maintainRepository(t.Context(), cfg, options, func(context.Context, config.Config) (storage.ObjectStore, error) {
		t.Fatal("opened storage for a namespace outside configured policy")
		return nil, nil
	}); err == nil {
		t.Fatal("accepted namespace outside configured policy")
	}
}

func TestMaintainRepositoryOperations(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"check", "generations", "restore", "gc", "repack", "lock", "unlock"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			objects, repositories, metadata := maintenanceFixture(t)
			options := RepositoryMaintenanceOptions{Operation: operation, Namespace: "alice", Repository: "demo"}
			if operation == "restore" {
				generations, err := repositories.ListGenerations(t.Context(), "alice", "demo")
				if err != nil || len(generations) != 1 {
					t.Fatalf("initial generations = %+v, error %v", generations, err)
				}
				options.Snapshot = generations[0].Snapshot
			}
			if operation == "lock" || operation == "unlock" {
				lock := repository.MaintenanceLock{Token: strings.Repeat("a", 32), CreatedAt: time.Now().UTC()}
				data, err := json.Marshal(lock)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := objects.Put(t.Context(), "repos/"+metadata.ID+"/maintenance-lock", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true}); err != nil {
					t.Fatal(err)
				}
				if operation == "unlock" {
					options.Token, options.Offline = lock.Token, true
				}
			}
			opened := false
			result, err := maintainRepository(t.Context(), maintenanceConfig(t, 1, 0), options, func(context.Context, config.Config) (storage.ObjectStore, error) {
				opened = true
				return objects, nil
			})
			if err != nil || !opened || result.Error != "" || result.Operation != operation || result.Report == nil {
				t.Fatalf("result = %+v, error = %v, opened = %v", result, err, opened)
			}
			if _, err := json.Marshal(result); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "check":
				report := result.Report.(repository.IntegrityReport)
				if report.Objects != 3 || report.Generation != 1 || report.RepositoryID != metadata.ID {
					t.Fatalf("integrity report = %+v", report)
				}
			case "generations":
				report := result.Report.([]repository.Generation)
				if len(report) != 1 || !report[0].Current {
					t.Fatalf("generations report = %+v", report)
				}
			case "restore":
				report := result.Report.(repository.RestoreReport)
				if report.Generation != 2 || report.SourceGeneration != 1 || report.SourceSnapshot != options.Snapshot {
					t.Fatalf("restore report = %+v", report)
				}
			case "gc":
				report := result.Report.(repository.GCReport)
				if !report.DryRun || report.Deleted != 0 || report.RetainedGenerations != 1 {
					t.Fatalf("gc report = %+v", report)
				}
			case "repack":
				report, err := repositories.CheckIntegrity(t.Context(), "alice", "demo")
				if err != nil || report.Packs != 1 || report.Generation != 2 || report.Objects != 3 {
					t.Fatalf("repacked integrity = %+v, error %v", report, err)
				}
			case "lock":
				report := result.Report.(repository.MaintenanceLock)
				if report.Token != strings.Repeat("a", 32) {
					t.Fatalf("lock report = %+v", report)
				}
			case "unlock":
				if _, err := repositories.GetMaintenanceLock(t.Context(), "alice", "demo"); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("lock after unlock = %v", err)
				}
			}
		})
	}
}

func TestMaintainRepositoryPreservesPartialGarbageCollection(t *testing.T) {
	t.Parallel()
	objects, _, metadata := maintenanceFixture(t)
	var keys []string
	for _, prefix := range []string{"aa", "bb"} {
		key := "repos/" + metadata.ID + "/objects/" + prefix + "/" + strings.Repeat("a", 38)
		if _, err := objects.Put(t.Context(), key, strings.NewReader("abc"), 3, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	wantErr := errors.New("delete denied")
	wrapped := &maintenanceDeleteFailure{ObjectStore: objects, failureKey: keys[1], err: wantErr}
	options := RepositoryMaintenanceOptions{Operation: "gc", Namespace: "alice", Repository: "demo", Apply: true}
	result, err := maintainRepository(t.Context(), maintenanceConfig(t, 1, 0), options, func(context.Context, config.Config) (storage.ObjectStore, error) {
		return wrapped, nil
	})
	if !errors.Is(err, wantErr) || result.Error == "" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	report, ok := result.Report.(repository.GCReport)
	if !ok || report.Deleted != 1 || report.DeletedBytes != 3 || report.Candidates != 2 || report.DryRun {
		t.Fatalf("partial report = %+v", result.Report)
	}
	if _, err := objects.Head(t.Context(), keys[0]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("first orphan was not removed: %v", err)
	}
	if _, err := objects.Head(t.Context(), keys[1]); err != nil {
		t.Fatalf("failed orphan disappeared: %v", err)
	}
}

func TestMaintainRepositoryOpeningFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("immutable identity mismatch")
	result, err := maintainRepository(t.Context(), maintenanceConfig(t, 1, 0), RepositoryMaintenanceOptions{
		Operation: "check", Namespace: "alice", Repository: "demo",
	}, func(context.Context, config.Config) (storage.ObjectStore, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) || result.Error != wantErr.Error() || result.Report != nil {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

type maintenanceDeleteFailure struct {
	storage.ObjectStore
	failureKey string
	err        error
}

func (s *maintenanceDeleteFailure) ListPage(ctx context.Context, prefix, after string, limit int) (storage.ObjectPage, error) {
	page, err := s.ObjectStore.ListPage(ctx, prefix, after, limit)
	for i := range page.Objects {
		page.Objects[i].LastModified = time.Unix(1, 0)
	}
	return page, err
}

func (s *maintenanceDeleteFailure) Delete(ctx context.Context, key string, version storage.Version) error {
	if key == s.failureKey {
		return s.err
	}
	return s.ObjectStore.Delete(ctx, key, version)
}

func maintenanceConfig(t *testing.T, count, local uint32) config.Config {
	t.Helper()
	values := map[string]string{
		"GITONE_SHARD_COUNT": fmt.Sprint(count), "POD_NAME": fmt.Sprintf("gitone-%d", local), "POD_NAMESPACE": "local",
	}
	cfg, err := config.Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func maintenanceFixture(t *testing.T) (*storage.MemoryStore, *repository.Store, repository.Metadata) {
	t.Helper()
	objects := storage.NewMemoryStore()
	repositories, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := repositories.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "operator", InitializeReadme: true,
		AuthorName: "Alice", AuthorEmail: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return objects, repositories, metadata
}
