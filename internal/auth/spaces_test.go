package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

type discoveryCountingStore struct {
	storage.ObjectStore
	gets         int
	lists        int
	pages        int
	pagePrefixes []string
}

func (s *discoveryCountingStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	s.gets++
	return s.ObjectStore.Get(ctx, key)
}

func (s *discoveryCountingStore) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.lists++
	return s.ObjectStore.List(ctx, prefix)
}

func (s *discoveryCountingStore) ListPage(ctx context.Context, prefix, after string, limit int) (storage.ObjectPage, error) {
	s.pages++
	s.pagePrefixes = append(s.pagePrefixes, prefix)
	return s.ObjectStore.ListPage(ctx, prefix, after, limit)
}

func initializeTestSpaceIndex(t *testing.T, s *Service) {
	t.Helper()
	if err := InitializeSpaceIndex(t.Context(), s.store, s.router, s.local); err != nil {
		t.Fatal(err)
	}
}

func localSpaceName(t *testing.T, s *Service, prefix string, index int) string {
	t.Helper()
	for candidate := index; ; candidate++ {
		name := fmt.Sprintf("%s-%06d", prefix, candidate)
		owner, err := s.router.Owner(name)
		if err != nil {
			t.Fatal(err)
		}
		if owner == s.local {
			return name
		}
	}
}

func TestSpacesDiscoveryDoesNotScanUnrelatedUsers(t *testing.T) {
	store := &discoveryCountingStore{ObjectStore: storage.NewMemoryStore()}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	initializeTestSpaceIndex(t, s)
	if _, err := s.createGroup(t.Context(), "acme", "google:alice-id"); err != nil {
		t.Fatal(err)
	}
	for candidate, count := 0, 0; count < 1000; candidate++ {
		name := fmt.Sprintf("unrelated-%d", candidate)
		owner, err := s.router.Owner(name)
		if err != nil {
			t.Fatal(err)
		}
		if owner != s.local {
			continue
		}
		if err := s.bindUser(t.Context(), name, Identity{Subject: name}); err != nil {
			t.Fatal(err)
		}
		count++
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	store.gets, store.lists, store.pages, store.pagePrefixes = 0, 0, 0, nil
	w := groupRequest(s, http.MethodGet, "/api/v1/spaces?shard=0", "", cookie, csrf)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"acme"`) {
		t.Fatalf("discovery status=%d: %s", w.Code, w.Body.String())
	}
	if store.gets != 2 || store.lists != 0 || store.pages != 1 || store.pagePrefixes[0] != spaceCandidatePrefix("google:alice-id") {
		t.Fatalf("discovery performed %d GETs, %d unbounded LISTs and %d pages (%v); want 2 GETs and one user-scoped page", store.gets, store.lists, store.pages, store.pagePrefixes)
	}
}

func TestSpacesPaginationAndAuthoritativeRoles(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	initializeTestSpaceIndex(t, s)
	stale := localSpaceName(t, s, "aaa", 0)
	group := localSpaceName(t, s, "zzz", 0)
	if err := s.ensureSpaceCandidate(t.Context(), stale, "google:bob-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createGroup(t.Context(), group, "google:alice-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateGroup(t.Context(), group, "google:alice-id", "invite", "google:bob-id", "reader"); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "bob", "bob-id")
	first := groupRequest(s, "GET", "/api/v1/spaces?shard=0&limit=1", "", cookie, csrf)
	var page struct {
		Spaces     []spaceView `json:"spaces"`
		NextCursor string      `json:"nextCursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || first.Code != 200 || len(page.Spaces) != 0 || page.NextCursor == "" {
		t.Fatalf("empty candidate page: %d %s, %v", first.Code, first.Body.String(), err)
	}
	path := "/api/v1/spaces?shard=0&limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	check := func(t *testing.T, wantRole string, invited bool) {
		t.Helper()
		w := groupRequest(s, "GET", path, "", cookie, csrf)
		page.NextCursor = ""
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || page.NextCursor != "" {
			t.Fatalf("page: %d %s, %v", w.Code, w.Body.String(), err)
		}
		if wantRole == "" {
			if len(page.Spaces) != 0 {
				t.Fatalf("revoked relationship disclosed: %+v", page.Spaces)
			}
			return
		}
		if len(page.Spaces) != 1 || page.Spaces[0].Name != group || page.Spaces[0].Role != wantRole || page.Spaces[0].Invited != invited {
			t.Fatalf("incorrect current relationship: %+v", page.Spaces)
		}
	}
	check(t, "reader", true)
	for _, change := range []struct {
		action, caller, role, wantRole string
		invited                        bool
	}{
		{"accept", "google:bob-id", "", "reader", false},
		{"set-role", "google:alice-id", "developer", "developer", false},
		{"remove", "google:alice-id", "", "", false},
		{"invite", "google:alice-id", "owner", "owner", true},
		{"cancel", "google:alice-id", "", "", false},
	} {
		t.Run(change.action, func(t *testing.T) {
			if _, err := s.updateGroup(t.Context(), group, change.caller, change.action, "google:bob-id", change.role); err != nil {
				t.Fatal(err)
			}
			check(t, change.wantRole, change.invited)
		})
	}
	if _, err := s.store.Head(t.Context(), spaceCandidatePrefix("google:bob-id")+group+".json"); err != nil {
		t.Fatalf("candidate must survive revocation/reinvite: %v", err)
	}
}

func TestSpaceCursorsAreScopedAndValidated(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	initializeTestSpaceIndex(t, s)
	id := "google:alice-id"
	key := spaceCandidatePrefix(id) + "acme.json"
	valid := spaceCursor{UserID: id, Shard: s.local, ShardCount: s.ShardCount(), Mode: s.spaceDiscoveryMode, Origin: s.origin, After: key}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	for _, test := range []struct {
		name   string
		change func(*spaceCursor)
	}{
		{"user", func(c *spaceCursor) { c.UserID = "google:bob-id" }},
		{"shard", func(c *spaceCursor) { c.Shard = 1 }},
		{"cluster", func(c *spaceCursor) { c.ShardCount++ }},
		{"mode", func(c *spaceCursor) { c.Mode = "scan" }},
		{"origin", func(c *spaceCursor) { c.Origin = "https://other.example" }},
		{"prefix", func(c *spaceCursor) { c.After = spaceCandidatePrefix("google:bob-id") + "acme.json" }},
		{"namespace", func(c *spaceCursor) { c.After = spaceCandidatePrefix(id) + "../acme.json" }},
		{"empty key", func(c *spaceCursor) { c.After = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cursor := valid
			test.change(&cursor)
			encoded, err := s.sessionCodec.Encode(spaceCursorLabel, cursor)
			if err != nil {
				t.Fatal(err)
			}
			w := groupRequest(s, "GET", "/api/v1/spaces?shard=0&cursor="+url.QueryEscape(encoded), "", cookie, csrf)
			if w.Code != 400 {
				t.Fatalf("foreign cursor accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, query := range []string{"cursor=tampered", "cursor=a&cursor=b", "limit=0", "limit=101", "limit=-1", "limit=a", "limit=1&limit=2"} {
		t.Run(query, func(t *testing.T) {
			w := groupRequest(s, "GET", "/api/v1/spaces?shard=0&"+query, "", cookie, csrf)
			if w.Code != 400 && w.Code != 422 {
				t.Fatalf("invalid query accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
	if w := groupRequest(s, "GET", "/api/v1/spaces?shard=0", "", nil, ""); w.Code != 401 {
		t.Fatalf("anonymous discovery accepted: %d", w.Code)
	}
}

type discoveryFaultStore struct {
	storage.ObjectStore
	putFailure func(string) error
	getFailure func(string) error
}

func (s *discoveryFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if s.putFailure != nil {
		if err := s.putFailure(key); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	return s.ObjectStore.Put(ctx, key, body, size, opts)
}

func (s *discoveryFaultStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if s.getFailure != nil {
		if err := s.getFailure(key); err != nil {
			return nil, storage.ObjectInfo{}, err
		}
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestSpaceCandidateWriteFailuresNeverPublishGrants(t *testing.T) {
	t.Parallel()
	failure := errors.New("storage unavailable")
	for _, operation := range []string{"create", "invite", "accept"} {
		t.Run(operation, func(t *testing.T) {
			store := &discoveryFaultStore{ObjectStore: storage.NewMemoryStore()}
			s := testService(t, 0, store, &fakeProvider{}, nil)
			initializeTestSpaceIndex(t, s)
			if operation != "create" {
				if _, err := s.createGroup(t.Context(), "acme", "google:alice-id"); err != nil {
					t.Fatal(err)
				}
			}
			if operation == "accept" {
				if _, err := s.updateGroup(t.Context(), "acme", "google:alice-id", "invite", "google:bob-id", "reader"); err != nil {
					t.Fatal(err)
				}
			}
			store.putFailure = func(key string) error {
				if strings.HasPrefix(key, spaceIndexRoot+"users/") {
					return failure
				}
				return nil
			}
			var err error
			switch operation {
			case "create":
				_, err = s.createGroup(t.Context(), "acme", "google:alice-id")
			case "invite":
				_, err = s.updateGroup(t.Context(), "acme", "google:alice-id", "invite", "google:bob-id", "reader")
			case "accept":
				_, err = s.updateGroup(t.Context(), "acme", "google:bob-id", "accept", "", "")
			}
			if !errors.Is(err, failure) {
				t.Fatalf("candidate failure ignored: %v", err)
			}
			record, _, err := s.loadNamespace(t.Context(), "acme")
			if operation == "create" {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("group published without candidate: %v", err)
				}
			} else if err != nil || record.Members["google:bob-id"] != "" || (operation == "invite" && record.Invitations["google:bob-id"] != "") {
				t.Fatalf("grant published despite candidate failure: %+v %v", record, err)
			}
		})
	}
}

func TestFailedGroupWritesLeaveOnlyHarmlessCandidates(t *testing.T) {
	t.Parallel()
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"storage failure", errors.New("storage unavailable")},
		{"CAS conflict", storage.ErrPreconditionFailed},
	} {
		t.Run(failure.name, func(t *testing.T) {
			store := &discoveryFaultStore{ObjectStore: storage.NewMemoryStore()}
			s := testService(t, 0, store, &fakeProvider{}, nil)
			initializeTestSpaceIndex(t, s)
			store.putFailure = func(key string) error {
				if key == namespaceKey("acme") {
					return failure.err
				}
				return nil
			}
			if _, err := s.createGroup(t.Context(), "acme", "google:alice-id"); err == nil {
				t.Fatal("failed group creation reported success")
			}
			cookie, csrf := groupSession(t, s, "alice", "alice-id")
			w := groupRequest(s, "GET", "/api/v1/spaces?shard=0", "", cookie, csrf)
			if w.Code != 200 || strings.Contains(w.Body.String(), "acme") {
				t.Fatalf("candidate granted membership: %d %s", w.Code, w.Body.String())
			}
			store.putFailure = nil
			if _, err := s.createGroup(t.Context(), "acme", "google:alice-id"); err != nil {
				t.Fatal(err)
			}
			// Replacing the service preserves discovery without an in-memory cache.
			restarted := testService(t, 0, store, &fakeProvider{}, nil)
			w = groupRequest(restarted, "GET", "/api/v1/spaces?shard=0", "", cookie, csrf)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "acme") {
				t.Fatalf("restart lost membership: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSpaceDiscoveryFailsVisiblyOnStorageErrors(t *testing.T) {
	t.Parallel()
	store := &discoveryFaultStore{ObjectStore: storage.NewMemoryStore()}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	initializeTestSpaceIndex(t, s)
	if _, err := s.createGroup(t.Context(), "acme", "google:alice-id"); err != nil {
		t.Fatal(err)
	}
	store.getFailure = func(key string) error {
		if key == namespaceKey("acme") {
			return errors.New("connection failed")
		}
		return nil
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	w := groupRequest(s, "GET", "/api/v1/spaces?shard=0", "", cookie, csrf)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"spaces"`) {
		t.Fatalf("storage failure silently hid groups: %d %s", w.Code, w.Body.String())
	}
}
