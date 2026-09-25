package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func groupSession(t *testing.T, s *Service, username, subject string) (*http.Cookie, string) {
	t.Helper()
	current := session{Username: username, Identity: Identity{Subject: subject}, Origin: s.origin,
		CSRF: "test-csrf-" + subject, Expires: time.Now().Add(time.Hour).Unix()}
	value, err := s.sessionCodec.Encode(sessionCookie, current)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}, current.CSRF
}

func groupRequest(handler http.Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(
		context.Background(),
		method,
		path,
		strings.NewReader(body),
	)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://git.example")
	r.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestGroupCreateInviteAcceptAndRoles(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	s := testService(t, 0, store, &fakeProvider{}, nil)
	ownerCookie, ownerCSRF := groupSession(t, s, "alice", "alice-id")
	memberCookie, memberCSRF := groupSession(t, s, "bob", "bob-id")
	outsiderCookie, outsiderCSRF := groupSession(t, s, "eve", "eve-id")
	check := func(handler http.Handler, method, path, body string, cookie *http.Cookie, csrf string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := groupRequest(handler, method, path, body, cookie, csrf)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	created := check(s, "POST", "/acme/", "", ownerCookie, ownerCSRF, 201)
	var view groupView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.CreatorUserID != "google:alice-id" || view.Members["google:alice-id"] != "owner" ||
		view.Type != groupNamespace || created.Header().Get("Location") != "/acme/" {
		t.Fatalf("invalid creation: %+v", view)
	}
	check(s, "POST", "/acme", "", memberCookie, memberCSRF, 409)
	check(s, "GET", "/acme", "", memberCookie, memberCSRF, 403)
	check(s, "POST", "/acme/invitations", `{"userId":"google:bob-id","role":"reader"}`, ownerCookie, ownerCSRF, 200)
	check(s, "GET", "/acme/repo.git/info/refs?service=git-upload-pack", "", memberCookie, memberCSRF, 403)
	check(s, "POST", "/acme/invitations/accept", "", outsiderCookie, outsiderCSRF, 404)
	check(s, "POST", "/acme/invitations/accept", "", memberCookie, memberCSRF, 200)
	check(s, "POST", "/acme/invitations/accept", "", memberCookie, memberCSRF, 404)
	check(s, "GET", "/acme/", "", memberCookie, memberCSRF, 200)
	check(s, "GET", "/acme/repo.git/info/refs?service=git-upload-pack", "", memberCookie, memberCSRF, 204)
	check(s, "POST", "/acme/repo.git/git-upload-pack", "stream", memberCookie, memberCSRF, 204)
	check(s, "GET", "/acme/repo.git/info/refs?service=git-receive-pack", "", memberCookie, memberCSRF, 403)
	check(s, "POST", "/acme/repo.git/git-receive-pack", "stream", memberCookie, memberCSRF, 403)
	check(s, "POST", "/acme/repo.git/info/lfs/objects/batch", `{"operation":"download","objects":[]}`, memberCookie, memberCSRF, 204)
	check(s, "POST", "/acme/repo.git/info/lfs/objects/batch", `{"operation":"upload","objects":[]}`, memberCookie, memberCSRF, 403)
	check(s, "POST", "/acme/invitations", `{"userId":"google:eve-id","role":"owner"}`, memberCookie, memberCSRF, 403)
	check(s, "PUT", "/acme/members", `{"userId":"google:bob-id","role":"owner"}`, memberCookie, memberCSRF, 403)
	check(s, "DELETE", "/acme/members", `{"userId":"google:alice-id"}`, memberCookie, memberCSRF, 403)
	check(s, "PUT", "/acme/members", `{"userId":"google:bob-id","role":"developer"}`, ownerCookie, ownerCSRF, 200)
	check(s, "POST", "/acme/repo.git/git-receive-pack", "stream", memberCookie, memberCSRF, 204)
	check(s, "GET", "/acme/repo.git/info/refs?service=git-receive-pack", "", memberCookie, memberCSRF, 204)
	check(s, "DELETE", "/acme/members", `{"userId":"google:alice-id"}`, ownerCookie, ownerCSRF, 409)
	check(s, "PUT", "/acme/members", `{"userId":"google:alice-id","role":"reader"}`, ownerCookie, ownerCSRF, 409)
	check(s, "PUT", "/acme/members", `{"userId":"google:bob-id","role":"owner"}`, ownerCookie, ownerCSRF, 200)
	check(s, "DELETE", "/acme/members", `{"userId":"google:alice-id"}`, memberCookie, memberCSRF, 200)
	check(s, "GET", "/acme/repo.git/info/refs?service=git-upload-pack", "", ownerCookie, ownerCSRF, 403)
	check(s, "POST", "/acme/invitations", `{"userId":"google:eve-id","role":"reader"}`, memberCookie, memberCSRF, 200)
	check(s, "DELETE", "/acme/invitations", `{"userId":"google:eve-id"}`, memberCookie, memberCSRF, 200)
	check(s, "POST", "/acme/invitations/accept", "", outsiderCookie, outsiderCSRF, 404)
	check(s, "DELETE", "/acme/", "", memberCookie, memberCSRF, 405)
	// Membership is read from durable storage even after replacing the pod.
	restarted := testService(t, 0, store, &fakeProvider{}, nil)
	check(restarted, "GET", "/acme", "", memberCookie, memberCSRF, 200)
	check(restarted, "GET", "/acme", "", ownerCookie, ownerCSRF, 403)
	check(restarted, "GET", "/acme/auth/google/login", "", nil, "", 409)
}

func TestGroupCreationRoutesToGroupShard(t *testing.T) {
	t.Parallel()
	userStore, groupStore := storage.NewMemoryStore(), storage.NewMemoryStore()
	provider := &fakeProvider{identity: Identity{Subject: "creator", Email: "creator@example.com"}}
	userShard := testService(t, 1, userStore, provider, nil)
	state, browser := startLogin(t, userShard)
	login := httptest.NewRecorder()
	userShard.ServeHTTP(login, callbackRequest(state, browser))
	cookie := responseCookie(t, login, sessionCookie)
	var current session
	if err := userShard.sessionCodec.Decode(sessionCookie, cookie.Value, &current); err != nil {
		t.Fatal(err)
	}
	groupShard := testService(t, 0, groupStore, &fakeProvider{fail: true}, nil)
	forwards := 0
	entry, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard: 1, Router: userShard, Next: userShard,
		Resolver: resolverFunc(func(id shard.ShardID) (*url.URL, error) {
			if id != 0 {
				t.Fatalf("wrong owner: %d", id)
			}
			return url.Parse("http://gitone-0.internal:8080")
		}),
		Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			forwards++
			w := httptest.NewRecorder()
			groupShard.ServeHTTP(w, r)
			return w.Result(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := groupRequest(entry, "POST", "/acme", "", cookie, current.CSRF)
	if w.Code != 201 || forwards != 1 {
		t.Fatalf("creation status=%d forwards=%d: %s", w.Code, forwards, w.Body.String())
	}
	if _, err := userStore.Head(context.Background(), namespaceKey("acme")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("group written to user's shard")
	}
	record, _, err := groupShard.loadNamespace(context.Background(), "acme")
	if err != nil || record.Members["google:creator"] != "owner" {
		t.Fatalf("missing owner: %+v %v", record, err)
	}
}

func TestSharedNamespaceClaimsAreAtomicAndLegacyCompatible(t *testing.T) {
	t.Parallel()
	for _, test := range []string{"legacy user", "new user", "group first", "concurrent user and group"} {
		t.Run(test, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := storage.NewMemoryStore()
			s := testService(t, 1, store, &fakeProvider{}, nil)
			switch test {
			case "legacy user":
				data := `{"subject":"old-google-id","email":"old@example.com"}`
				if _, err := store.Put(ctx, namespaceKey("alice"), strings.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true}); err != nil {
					t.Fatal(err)
				}
				if err := s.bindUser(ctx, "alice", Identity{Subject: "old-google-id", Email: "new@example.com"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.createGroup(ctx, "alice", "google:creator"); !errors.Is(err, errNamespaceTaken) {
					t.Fatalf("claimed legacy user: %v", err)
				}
			case "new user":
				if err := s.bindUser(ctx, "alice", Identity{Subject: "alice"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.createGroup(ctx, "alice", "google:creator"); !errors.Is(err, errNamespaceTaken) {
					t.Fatalf("claimed user: %v", err)
				}
			case "group first":
				if _, err := s.createGroup(ctx, "alice", "google:creator"); err != nil {
					t.Fatal(err)
				}
				if err := s.bindUser(ctx, "alice", Identity{Subject: "creator"}); !errors.Is(err, errUsernameTaken) {
					t.Fatalf("claimed group: %v", err)
				}
			case "concurrent user and group":
				var successes atomic.Int32
				var wg sync.WaitGroup
				for i := range 16 {
					wg.Go(func() {
						var err error
						if i%2 == 0 {
							err = s.bindUser(ctx, "alice", Identity{Subject: fmt.Sprintf("user-%d", i)})
						} else {
							_, err = s.createGroup(ctx, "alice", fmt.Sprintf("google:creator-%d", i))
						}
						if err == nil {
							successes.Add(1)
						} else if !errors.Is(err, errUsernameTaken) && !errors.Is(err, errNamespaceTaken) {
							t.Errorf("unexpected claim error: %v", err)
						}
					})
				}
				wg.Wait()
				if successes.Load() != 1 {
					t.Fatalf("successful claims=%d", successes.Load())
				}
			}
		})
	}
}

func TestGroupInputAndSessionChecks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, method, path, body string
		anonymous, noCSRF        bool
		want                     int
	}{
		{name: "anonymous create", method: "POST", path: "/acme", anonymous: true, want: 401},
		{name: "missing csrf create", method: "POST", path: "/acme", noCSRF: true, want: 403},
		{name: "reserved", method: "POST", path: "/auth", want: 400},
		{name: "invalid name", method: "POST", path: "/Acme", want: 400},
		{name: "traversal", method: "POST", path: "/../acme", want: 400},
		{name: "missing invite csrf", method: "POST", path: "/acme/invitations", body: `{"userId":"google:bob","role":"reader"}`, noCSRF: true, want: 403},
		{name: "unknown role", method: "POST", path: "/acme/invitations", body: `{"userId":"google:bob","role":"admin"}`, want: 400},
		{name: "email instead of id", method: "POST", path: "/acme/invitations", body: `{"userId":"bob@example.com","role":"reader"}`, want: 400},
		{name: "unknown field", method: "POST", path: "/acme/invitations", body: `{"userId":"google:bob","role":"reader","owner":true}`, want: 400},
		{name: "trailing JSON", method: "POST", path: "/acme/invitations", body: `{"userId":"google:bob","role":"reader"}{}`, want: 400},
		{name: "oversized body", method: "POST", path: "/acme/invitations", body: strings.Repeat(" ", 4097) + `{}`, want: 400},
		{name: "no group rename", method: "PUT", path: "/acme", body: `{"name":"other"}`, want: 405},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
			cookie, csrf := groupSession(t, s, "alice", "alice-id")
			if _, err := s.createGroup(context.Background(), "acme", "google:alice-id"); err != nil {
				t.Fatal(err)
			}
			if test.anonymous {
				cookie = nil
			}
			if test.noCSRF {
				csrf = ""
			}
			w := groupRequest(s, test.method, test.path, test.body, cookie, csrf)
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, test.want, w.Body.String())
			}
		})
	}
}

type coordinatedStore struct {
	storage.ObjectStore
	key      string
	reads    atomic.Int32
	bothRead chan struct{}
}

func (s *coordinatedStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, key)
	if key == s.key && err == nil {
		n := s.reads.Add(1)
		if n == 2 {
			close(s.bothRead)
		}
		if n <= 2 {
			select {
			case <-s.bothRead:
			case <-ctx.Done():
				_ = body.Close()
				return nil, storage.ObjectInfo{}, ctx.Err()
			}
		}
	}
	return body, info, err
}

func TestConcurrentOwnerRemovalKeepsOneOwner(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &coordinatedStore{ObjectStore: storage.NewMemoryStore(), key: namespaceKey("acme"), bothRead: make(chan struct{})}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:a",
		Members: map[string]string{"google:a": "owner", "google:b": "owner"}}
	if err := s.writeNamespace(ctx, "acme", record, ""); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, caller := range []string{"google:a", "google:b"} {
		wg.Go(func() {
			_, err := s.updateGroup(ctx, "acme", caller, "remove", caller, "")
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, errLastOwner) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	remaining, _, err := s.loadNamespace(ctx, "acme")
	if err != nil || successes.Load() != 1 || len(remaining.Members) != 1 {
		t.Fatalf("remaining=%+v successes=%d err=%v", remaining, successes.Load(), err)
	}
}

type staleReadStore struct {
	storage.ObjectStore
	first  atomic.Bool
	read   chan struct{}
	resume chan struct{}
}

func (s *staleReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, key)
	if err == nil && s.first.CompareAndSwap(false, true) {
		close(s.read)
		select {
		case <-s.resume:
		case <-ctx.Done():
			_ = body.Close()
			return nil, storage.ObjectInfo{}, ctx.Err()
		}
	}
	return body, info, err
}

func TestGroupUpdateRechecksRevokedOwnerAfterConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := storage.NewMemoryStore()
	blocked := &staleReadStore{ObjectStore: base, read: make(chan struct{}), resume: make(chan struct{})}
	staleOwner := testService(t, 0, blocked, &fakeProvider{}, nil)
	otherOwner := testService(t, 0, base, &fakeProvider{}, nil)
	record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:a",
		Members: map[string]string{"google:a": "owner", "google:b": "owner"}}
	if err := otherOwner.writeNamespace(ctx, "acme", record, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := staleOwner.updateGroup(ctx, "acme", "google:a", "invite", "google:eve", "owner")
		done <- err
	}()
	select {
	case <-blocked.read:
	case <-ctx.Done():
		t.Fatal("owner did not read group")
	}
	if _, err := otherOwner.updateGroup(ctx, "acme", "google:b", "remove", "google:a", ""); err != nil {
		t.Fatal(err)
	}
	close(blocked.resume)
	select {
	case err := <-done:
		if !errors.Is(err, errGroupDenied) {
			t.Fatalf("revoked owner update = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("update did not finish")
	}
	final, _, err := otherOwner.loadNamespace(ctx, "acme")
	if err != nil || final.Invitations["google:eve"] != "" || final.Members["google:a"] != "" {
		t.Fatalf("revoked owner overwrote group: %+v %v", final, err)
	}
}

func TestConcurrentGroupInvitesDoNotLoseUpdates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &coordinatedStore{ObjectStore: storage.NewMemoryStore(), key: namespaceKey("acme"), bothRead: make(chan struct{})}
	s := testService(t, 0, store, &fakeProvider{}, nil)
	if _, err := s.createGroup(ctx, "acme", "google:owner"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, target := range []string{"google:a", "google:b"} {
		wg.Go(func() {
			if _, err := s.updateGroup(ctx, "acme", "google:owner", "invite", target, "reader"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	record, _, err := s.loadNamespace(ctx, "acme")
	if err != nil || len(record.Invitations) != 2 {
		t.Fatalf("lost invitations: %+v %v", record, err)
	}
}

type unavailableNamespaceStore struct{ storage.ObjectStore }

func (unavailableNamespaceStore) Get(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, errors.New("store unavailable")
}
func (unavailableNamespaceStore) Put(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, errors.New("store unavailable")
}

func TestGroupStorageFailureDoesNotGrantAccess(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, unavailableNamespaceStore{}, &fakeProvider{}, nil)
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			w := groupRequest(s, method, "/acme", "", cookie, csrf)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
		})
	}
}

func TestGroupLFSReadAuthorizationMatchesForwardedBody(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"download", `{"operation":"download","objects":[{"oid":"abc","size":3}]}`, 204},
		{"duplicate normalized", `{"operation":"upload","operation":"download","objects":[]}`, 204},
		{"upload last", `{"operation":"download","operation":"upload","objects":[]}`, 403},
		{"case ambiguity", `{"operation":"download","Operation":"upload","objects":[]}`, 400},
		{"invalid operation", `{"operation":"delete","objects":[]}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var batch struct {
					Operation string
					Objects   []json.RawMessage
				}
				if err := json.Unmarshal(data, &batch); err != nil {
					t.Error(err)
				}
				if batch.Operation != "download" || int64(len(data)) != r.ContentLength {
					t.Errorf("unsafe forwarded batch: %s", data)
				}
				if test.name == "download" && len(batch.Objects) != 1 {
					t.Error("batch objects lost")
				}
				w.WriteHeader(204)
			})
			s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, next)
			record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:owner",
				Members: map[string]string{"google:owner": "owner", "google:reader": "reader"}}
			if err := s.writeNamespace(context.Background(), "acme", record, ""); err != nil {
				t.Fatal(err)
			}
			cookie, csrf := groupSession(t, s, "alice", "reader")
			w := groupRequest(s, "POST", "/acme/repo.git/info/lfs/objects/batch", test.body, cookie, csrf)
			if w.Code != test.want || called != (test.want == 204) {
				t.Fatalf("status=%d called=%t", w.Code, called)
			}
		})
	}
}
