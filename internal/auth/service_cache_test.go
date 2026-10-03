package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type cacheReadStore struct {
	storage.ObjectStore
	immutableReads atomic.Int64
	authorityReads atomic.Int64
}

func (s *cacheReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/states/") || strings.Contains(key, "/objects/") || strings.Contains(key, "/packs/") {
		s.immutableReads.Add(1)
	}
	if strings.HasPrefix(key, "auth/users/") {
		s.authorityReads.Add(1)
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestBrowserSharesRepositoryCacheAndRechecksAuthority(t *testing.T) {
	t.Parallel()
	shared, err := cache.New(cache.Options{MemoryBytes: 1 << 20, Namespace: "shared-browser-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shared.Close(); err != nil {
			t.Error(err)
		}
	})
	objects := &cacheReadStore{ObjectStore: storage.NewMemoryStore()}
	uncached := testService(t, 1, objects, &fakeProvider{}, nil)
	service, err := New(Options{Config: testConfig(), LocalShard: uncached.local, Router: uncached.router,
		Store: objects, Provider: &fakeProvider{}, Next: uncached.next, RepositoryCache: shared})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.bindUser(t.Context(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := groupSession(t, service, "alice", "alice-id")
	protocolStore, err := repository.New(objects, repository.WithCache(shared))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocolStore.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "google:alice-id", InitializeReadme: true,
		AuthorName: "alice", AuthorEmail: "alice@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := protocolStore.Browse(t.Context(), "alice", "demo", "", "", false); err != nil {
		t.Fatal(err)
	}
	before, authorityBefore := objects.immutableReads.Load(), objects.authorityReads.Load()
	if before == 0 {
		t.Fatal("fixture did not load immutable repository content")
	}
	response := browserAdmissionRequest(t.Context(), service, cookie, "/api/v1/repos/alice/demo/browse")
	if response.Code != http.StatusOK {
		t.Fatalf("browser status = %d: %s", response.Code, response.Body)
	}
	if got := objects.immutableReads.Load(); got != before {
		t.Fatalf("browser did not reuse protocol cache: immutable reads %d -> %d", before, got)
	}
	if objects.authorityReads.Load() <= authorityBefore {
		t.Fatal("browser cache skipped current namespace authorization")
	}
	info, err := objects.Head(t.Context(), namespaceKey("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(t.Context(), namespaceKey("alice"), info.Version); err != nil {
		t.Fatal(err)
	}
	response = browserAdmissionRequest(t.Context(), service, cookie, "/api/v1/repos/alice/demo/browse")
	if response.Code == http.StatusOK {
		t.Fatal("cached private repository survived removal of namespace authority")
	}
}

func TestWarmBrowserCacheRejectsRevokedGroupMember(t *testing.T) {
	t.Parallel()
	shared, err := cache.New(cache.Options{MemoryBytes: 1 << 20, Namespace: "group-revocation-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shared.Close(); err != nil {
			t.Error(err)
		}
	})
	objects := &cacheReadStore{ObjectStore: storage.NewMemoryStore()}
	uncached := testService(t, 0, objects, &fakeProvider{}, nil)
	service, err := New(Options{Config: testConfig(), LocalShard: uncached.local, Router: uncached.router,
		Store: objects, Provider: &fakeProvider{}, Next: uncached.next, RepositoryCache: shared})
	if err != nil {
		t.Fatal(err)
	}
	ownerCookie, ownerCSRF := groupSession(t, service, "alice", "alice-id")
	memberCookie, memberCSRF := groupSession(t, service, "bob", "bob-id")
	owner := repositoryRequestChecker(t, service, ownerCookie, ownerCSRF)
	member := repositoryRequestChecker(t, service, memberCookie, memberCSRF)
	owner("POST", "/api/v1/groups/acme", "", http.StatusCreated)
	owner("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:bob-id","role":"reader"}`, http.StatusOK)
	member("POST", "/api/v1/groups/acme/invitations/accept", "", http.StatusOK)
	owner("POST", "/api/v1/repos/acme", `{"name":"private","initializeReadme":true}`, http.StatusCreated)
	path := "/api/v1/repos/acme/private/browse"
	member("GET", path, "", http.StatusOK)
	reads := objects.immutableReads.Load()
	member("GET", path, "", http.StatusOK)
	if reads == 0 || objects.immutableReads.Load() != reads {
		t.Fatal("fixture did not warm the shared browser cache")
	}
	owner("DELETE", "/api/v1/groups/acme/members", `{"userId":"google:bob-id"}`, http.StatusOK)
	member("GET", path, "", http.StatusForbidden)
	member("GET", "/api/v1/repos/acme/private/blob?path=README.md", "", http.StatusForbidden)
	owner("GET", path, "", http.StatusOK)
	if objects.immutableReads.Load() != reads {
		t.Fatal("revocation or remaining owner access discarded the immutable cache")
	}
}
