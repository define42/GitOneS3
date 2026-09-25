package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

type spaceMigrationStore struct {
	storage.ObjectStore
	beforePut  func(context.Context, string) error
	beforeGet  func(context.Context, string) error
	beforePage func(context.Context, string, string, int) error
}

func (s *spaceMigrationStore) Put(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
	opts storage.PutOptions,
) (storage.ObjectInfo, error) {
	if s.beforePut != nil {
		if err := s.beforePut(ctx, key); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	return s.ObjectStore.Put(ctx, key, body, size, opts)
}

func (s *spaceMigrationStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.beforeGet != nil {
		if err := s.beforeGet(ctx, key); err != nil {
			return nil, storage.ObjectInfo{}, err
		}
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *spaceMigrationStore) List(context.Context, string) ([]storage.ObjectInfo, error) {
	return nil, errors.New("migration must not use unbounded listing")
}

func (s *spaceMigrationStore) ListPage(
	ctx context.Context,
	prefix, after string,
	limit int,
) (storage.ObjectPage, error) {
	if s.beforePage != nil {
		if err := s.beforePage(ctx, prefix, after, limit); err != nil {
			return storage.ObjectPage{}, err
		}
	}
	return s.ObjectStore.ListPage(ctx, prefix, after, limit)
}

func seedLegacyMigrationGroups(t *testing.T, s *Service, count int) []string {
	t.Helper()
	names := make([]string, 0, count)
	for candidate := 0; len(names) < count; candidate++ {
		name := fmt.Sprintf("legacy-team-%06d", candidate)
		owner, err := s.router.Owner(name)
		if err != nil {
			t.Fatal(err)
		}
		if owner != s.local {
			continue
		}
		record := namespaceRecord{
			SchemaVersion: 1,
			Type:          groupNamespace,
			CreatorUserID: "google:owner-id",
			Members: map[string]string{
				"google:owner-id":  "owner",
				"google:member-id": "developer",
			},
			Invitations: map[string]string{"google:invited-id": "reader"},
		}
		// Simulate pre-index binaries: publish the authoritative record directly,
		// without creating any discovery candidates.
		if err := s.writeNamespace(t.Context(), name, record, ""); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func requireMigrationNotReady(t *testing.T, store storage.ObjectStore) {
	t.Helper()
	if _, err := store.Head(t.Context(), spaceIndexReadyKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("readiness marker exists before migration completed: %v", err)
	}
}

func migrationSpaces(t *testing.T, s *Service, id string) []spaceView {
	t.Helper()
	result := []spaceView{}
	after := ""
	for {
		spaces, next, err := s.listSpaces(t.Context(), id, after, spacePageSize)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, spaces...)
		if next == "" {
			return result
		}
		if next <= after {
			t.Fatalf("discovery cursor did not advance: %q -> %q", after, next)
		}
		after = next
	}
}

func requireMigratedGroups(t *testing.T, s *Service, names []string) {
	t.Helper()
	for _, relationship := range []struct {
		id      string
		role    string
		invited bool
	}{
		{id: "google:owner-id", role: "owner"},
		{id: "google:member-id", role: "developer"},
		{id: "google:invited-id", role: "reader", invited: true},
	} {
		spaces := migrationSpaces(t, s, relationship.id)
		if len(spaces) != len(names) {
			t.Fatalf("%s discovers %d spaces, want %d", relationship.id, len(spaces), len(names))
		}
		for index, space := range spaces {
			if space.Name != names[index] || space.Role != relationship.role || space.Invited != relationship.invited {
				t.Fatalf("%s has incorrect migrated relationship: %+v", relationship.id, space)
			}
		}
	}
}

func TestInitializeSpaceIndexEmptyShard(t *testing.T) {
	t.Parallel()
	base := storage.NewMemoryStore()
	store := &spaceMigrationStore{ObjectStore: base}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	pages := 0
	store.beforePage = func(_ context.Context, prefix, after string, limit int) error {
		pages++
		if prefix != "auth/users/" || after != "" || limit != 1 {
			t.Fatalf("empty-shard check is not bounded: prefix=%q after=%q limit=%d", prefix, after, limit)
		}
		return nil
	}
	initializeTestSpaceIndex(t, s)
	ready, err := s.spaceIndexReady(t.Context())
	if err != nil || !ready || pages != 1 {
		t.Fatalf("empty shard readiness=%v error=%v page calls=%d", ready, err, pages)
	}
	before, err := base.Head(t.Context(), spaceIndexReadyKey)
	if err != nil {
		t.Fatal(err)
	}
	initializeTestSpaceIndex(t, s)
	after, err := base.Head(t.Context(), spaceIndexReadyKey)
	if err != nil || after.Version != before.Version || pages != 1 {
		t.Fatalf("initialization was not idempotent: before=%+v after=%+v pages=%d error=%v", before, after, pages, err)
	}
}

func TestInitializeSpaceIndexRefusesExistingNamespaces(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{userNamespace, groupNamespace} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			base := storage.NewMemoryStore()
			store := &spaceMigrationStore{ObjectStore: base}
			s := testService(t, 0, store, &fakeProvider{}, nil)
			if kind == groupNamespace {
				seedLegacyMigrationGroups(t, s, 1)
			} else {
				name := localSpaceName(t, s, "legacy-user", 0)
				if err := s.writeNamespace(t.Context(), name, namespaceRecord{
					SchemaVersion: 1,
					Type:          userNamespace,
					Identity:      Identity{Subject: "legacy-user-id"},
				}, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := InitializeSpaceIndex(t.Context(), store, s.router, s.local); !errors.Is(err, ErrSpaceIndexNotReady) {
				t.Fatalf("nonempty shard initialization error = %v, want ErrSpaceIndexNotReady", err)
			}
			requireMigrationNotReady(t, base)
		})
	}
}

func TestInitializeSpaceIndexRejectsInvalidReadiness(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "wrong shard count", data: `{"schemaVersion":1,"shard":0,"shardCount":3}`},
		{name: "wrong shard", data: `{"schemaVersion":1,"shard":1,"shardCount":2}`},
		{name: "wrong schema", data: `{"schemaVersion":2,"shard":0,"shardCount":2}`},
		{name: "corrupt json", data: `{`},
		{name: "empty marker", data: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base := storage.NewMemoryStore()
			store := &spaceMigrationStore{ObjectStore: base}
			s := testService(t, 0, store, &fakeProvider{}, nil)
			names := seedLegacyMigrationGroups(t, s, 1)
			before, err := base.Put(
				t.Context(),
				spaceIndexReadyKey,
				strings.NewReader(test.data),
				int64(len(test.data)),
				storage.PutOptions{IfNoneMatch: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := InitializeSpaceIndex(t.Context(), store, s.router, s.local); err == nil {
				t.Fatal("initialization accepted invalid readiness")
			}
			if _, _, err := s.listSpaces(t.Context(), "google:owner-id", "", 100); err == nil {
				t.Fatal("discovery accepted invalid readiness")
			}
			after, err := base.Head(t.Context(), spaceIndexReadyKey)
			if err != nil || after.Version != before.Version {
				t.Fatalf("automatic initialization overwrote invalid readiness: %v", err)
			}
			if err := BackfillSpaceIndex(t.Context(), store, s.router, s.local); err != nil {
				t.Fatalf("explicit backfill could not rebuild invalid readiness: %v", err)
			}
			requireMigratedGroups(t, s, names)
		})
	}
}

func TestBackfillSpaceIndexPaginationAndIdempotence(t *testing.T) {
	t.Parallel()
	base := storage.NewMemoryStore()
	store := &spaceMigrationStore{ObjectStore: base}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	names := seedLegacyMigrationGroups(t, s, 121)
	user := localSpaceName(t, s, "legacy-user", 0)
	if err := s.writeNamespace(t.Context(), user, namespaceRecord{
		SchemaVersion: 1,
		Type:          userNamespace,
		Identity:      Identity{Subject: "personal-user-id"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google:owner-id", "google:member-id", "google:invited-id"} {
		page, err := base.ListPage(t.Context(), spaceCandidatePrefix(id), "", 1)
		if err != nil || len(page.Objects) != 0 {
			t.Fatalf("legacy fixture unexpectedly has candidates: %+v, %v", page, err)
		}
	}
	originalVersions := make(map[string]storage.Version, len(names)+1)
	for _, name := range append(slices.Clone(names), user) {
		info, err := base.Head(t.Context(), namespaceKey(name))
		if err != nil {
			t.Fatal(err)
		}
		originalVersions[name] = info.Version
	}
	pageCursors := []string{}
	readyWrites := 0
	store.beforePage = func(_ context.Context, prefix, after string, limit int) error {
		if prefix == "auth/users/" {
			requireMigrationNotReady(t, base)
			if limit != spacePageSize {
				t.Fatalf("migration page limit = %d, want %d", limit, spacePageSize)
			}
			pageCursors = append(pageCursors, after)
		}
		return nil
	}
	store.beforePut = func(_ context.Context, key string) error {
		requireMigrationNotReady(t, base)
		if key != spaceIndexReadyKey {
			return nil
		}
		readyWrites++
		if len(pageCursors) != 2 {
			t.Fatalf("readiness published after %d pages, want 2", len(pageCursors))
		}
		for _, name := range names {
			for _, id := range []string{"google:owner-id", "google:member-id", "google:invited-id"} {
				if _, err := base.Head(t.Context(), spaceCandidatePrefix(id)+name+".json"); err != nil {
					t.Fatalf("readiness published before candidate %s/%s: %v", id, name, err)
				}
			}
		}
		return nil
	}
	if err := BackfillSpaceIndex(t.Context(), store, s.router, s.local); err != nil {
		t.Fatal(err)
	}
	if readyWrites != 1 || len(pageCursors) != 2 || pageCursors[0] != "" || pageCursors[1] != namespaceKey(names[99]) {
		t.Fatalf("unexpected readiness or page sequence: writes=%d cursors=%v", readyWrites, pageCursors)
	}
	store.beforePut, store.beforePage = nil, nil
	requireMigratedGroups(t, s, names)
	if spaces := migrationSpaces(t, s, "google:personal-user-id"); len(spaces) != 0 {
		t.Fatalf("personal namespace was indexed as a group: %+v", spaces)
	}
	candidates, err := base.ListPage(t.Context(), spaceCandidatePrefix("google:owner-id"), "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := BackfillSpaceIndex(t.Context(), store, s.router, s.local); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	requireMigratedGroups(t, s, names)
	after, err := base.ListPage(t.Context(), spaceCandidatePrefix("google:owner-id"), "", 1000)
	if err != nil || !slices.Equal(candidates.Objects, after.Objects) {
		t.Fatalf("rerun rewrote existing candidate objects: %v", err)
	}
	for name, version := range originalVersions {
		info, err := base.Head(t.Context(), namespaceKey(name))
		if err != nil || info.Version != version {
			t.Fatalf("backfill modified authoritative namespace %s: %v", name, err)
		}
	}
}

func TestBackfillSpaceIndexFailuresDoNotLeaveReadyAndCanRetry(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"candidate write", "group read", "later page", "cancellation", "readiness write"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			base := storage.NewMemoryStore()
			store := &spaceMigrationStore{ObjectStore: base}
			s := testService(t, 0, store, &fakeProvider{}, nil)
			initializeTestSpaceIndex(t, s)
			count := 2
			if stage == "later page" {
				count = 101
			}
			names := seedLegacyMigrationGroups(t, s, count)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("migration storage unavailable")
			injected := false
			store.beforePut = func(_ context.Context, key string) error {
				requireMigrationNotReady(t, base)
				if stage == "readiness write" && key == spaceIndexReadyKey {
					injected = true
					return failure
				}
				if !strings.HasPrefix(key, spaceIndexRoot+"users/") {
					return nil
				}
				if stage == "candidate write" {
					injected = true
					return failure
				}
				if stage == "cancellation" {
					injected = true
					cancel()
				}
				return nil
			}
			store.beforeGet = func(_ context.Context, key string) error {
				if stage == "group read" && key == namespaceKey(names[len(names)-1]) {
					injected = true
					return failure
				}
				return nil
			}
			store.beforePage = func(_ context.Context, prefix, after string, _ int) error {
				if stage == "later page" && prefix == "auth/users/" && after != "" {
					injected = true
					return failure
				}
				return nil
			}
			wantErr := failure
			if stage == "cancellation" {
				wantErr = context.Canceled
			}
			if err := BackfillSpaceIndex(ctx, store, s.router, s.local); !errors.Is(err, wantErr) || !injected {
				t.Fatalf("backfill error = %v, injected=%v, want %v", err, injected, wantErr)
			}
			requireMigrationNotReady(t, base)
			store.beforePut, store.beforeGet, store.beforePage = nil, nil, nil
			if _, _, err := s.listSpaces(t.Context(), "google:owner-id", "", 100); !errors.Is(err, ErrSpaceIndexNotReady) {
				t.Fatalf("failed migration discovery error = %v, want ErrSpaceIndexNotReady", err)
			}
			if err := BackfillSpaceIndex(t.Context(), store, s.router, s.local); err != nil {
				t.Fatalf("retry after %s failed: %v", stage, err)
			}
			requireMigratedGroups(t, s, names)
		})
	}
}

func TestBackfillSpaceIndexPreservesWritesBehindCursor(t *testing.T) {
	t.Parallel()
	base := storage.NewMemoryStore()
	store := &spaceMigrationStore{ObjectStore: base}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	// Migration deployment runs index-aware writers in scan mode until every
	// shard has finished its backfill.
	s.spaceDiscoveryMode = "scan"
	names := seedLegacyMigrationGroups(t, s, 101)
	newGroup := localSpaceName(t, s, "aaa-new-team", 0)
	inserted := false
	store.beforePage = func(ctx context.Context, prefix, after string, _ int) error {
		if prefix != "auth/users/" || after == "" || inserted {
			return nil
		}
		inserted = true
		requireMigrationNotReady(t, base)
		if namespaceKey(newGroup) >= after || namespaceKey(names[0]) >= after {
			t.Fatal("fixture writes are not behind the migration cursor")
		}
		// Interleave deterministically between storage pages. No sleeps or shared
		// mutable state between goroutines are needed to exercise the race window.
		if _, err := s.createGroup(ctx, newGroup, "google:new-owner-id"); err != nil {
			return err
		}
		if _, err := s.updateGroup(ctx, newGroup, "google:new-owner-id", "invite", "google:new-invited-id", "reader"); err != nil {
			return err
		}
		if _, err := s.updateGroup(ctx, names[0], "google:owner-id", "invite", "google:late-member-id", "developer"); err != nil {
			return err
		}
		_, err := s.updateGroup(ctx, names[0], "google:late-member-id", "accept", "", "")
		return err
	}
	if err := BackfillSpaceIndex(t.Context(), store, s.router, s.local); err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("backfill did not cross the required page boundary")
	}
	store.beforePage = nil
	s.spaceDiscoveryMode = "indexed"
	requireMigratedGroups(t, s, names)
	for _, test := range []struct {
		id      string
		name    string
		role    string
		invited bool
	}{
		{id: "google:new-owner-id", name: newGroup, role: "owner"},
		{id: "google:new-invited-id", name: newGroup, role: "reader", invited: true},
		{id: "google:late-member-id", name: names[0], role: "developer"},
	} {
		spaces := migrationSpaces(t, s, test.id)
		if len(spaces) != 1 || spaces[0].Name != test.name || spaces[0].Role != test.role || spaces[0].Invited != test.invited {
			t.Fatalf("writer behind migration cursor lost visibility for %s: %+v", test.id, spaces)
		}
	}
}
