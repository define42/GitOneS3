package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestLFSCredentialRules(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id"}
	if err := s.bindUser(t.Context(), "alice", identity); err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string)
	for _, permission := range []string{"read", "write"} {
		raw, _, err := s.createToken(t.Context(), "alice", identity, "LFS", permission, []string{"alice/project"}, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		tokens[permission] = raw
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	oid := strings.Repeat("a", 64)
	for _, test := range []struct {
		name, method, endpoint, body, permission string
		status                                   int
		cookie, csrf                             bool
	}{
		{name: "anonymous", method: "GET", endpoint: "objects/" + oid, status: 401},
		{name: "read batch", method: "POST", endpoint: "objects/batch", body: `{"operation":"download","objects":[]}`, permission: "read", status: 204},
		{name: "read bytes", method: "GET", endpoint: "objects/" + oid, permission: "read", status: 204},
		{name: "read head", method: "HEAD", endpoint: "objects/" + oid, permission: "read", status: 204},
		{name: "reader cannot upload batch", method: "POST", endpoint: "objects/batch", body: `{"operation":"upload","objects":[]}`, permission: "read", status: 403},
		{name: "reader cannot upload bytes", method: "PUT", endpoint: "objects/" + oid, body: "payload", permission: "read", status: 403},
		{name: "reader cannot verify upload", method: "POST", endpoint: "objects/" + oid + "/verify", body: "{}", permission: "read", status: 403},
		{name: "writer upload batch", method: "POST", endpoint: "objects/batch", body: `{"operation":"upload","objects":[]}`, permission: "write", status: 204},
		{name: "writer upload bytes", method: "PUT", endpoint: "objects/" + oid, body: "payload", permission: "write", status: 204},
		{name: "writer verify", method: "POST", endpoint: "objects/" + oid + "/verify", body: "{}", permission: "write", status: 204},
		{name: "malformed operation", method: "POST", endpoint: "objects/batch", body: `{"operation":true}`, permission: "read", status: 400},
		{name: "missing operation", method: "POST", endpoint: "objects/batch", body: `{"objects":[]}`, permission: "read", status: 400},
		{name: "duplicate operation", method: "POST", endpoint: "objects/batch", body: `{"operation":"upload","operation":"download"}`, permission: "write", status: 400},
		{name: "case alias", method: "POST", endpoint: "objects/batch", body: `{"operation":"download","Operation":"upload"}`, permission: "write", status: 400},
		{name: "null operation", method: "POST", endpoint: "objects/batch", body: `{"operation":null}`, permission: "write", status: 400},
		{name: "trailing document", method: "POST", endpoint: "objects/batch", body: `{"operation":"download"}{}`, permission: "write", status: 400},
		{name: "array request", method: "POST", endpoint: "objects/batch", body: `["download"]`, permission: "write", status: 400},
		{name: "oversized batch", method: "POST", endpoint: "objects/batch", body: strings.Repeat(" ", maxLFSBatchBytes+1), permission: "write", status: 413},
		{name: "cookie download POST needs CSRF", method: "POST", endpoint: "objects/batch", body: `{"operation":"download"}`, cookie: true, status: 403},
		{name: "cookie valid CSRF", method: "POST", endpoint: "objects/batch", body: `{"operation":"download"}`, cookie: true, csrf: true, status: 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), test.method, "/alice/project.git/info/lfs/"+test.endpoint, strings.NewReader(test.body))
			if test.permission != "" {
				r.SetBasicAuth("alice", tokens[test.permission])
			}
			if test.cookie {
				r.AddCookie(cookie)
			}
			if test.csrf {
				r.Header.Set("Origin", s.origin)
				r.Header.Set("X-CSRF-Token", csrf)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.status, w.Body.String())
			}
		})
	}
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/alice/other.git/info/lfs/objects/"+oid, nil)
	r.SetBasicAuth("alice", tokens["read"])
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("PAT repository scope bypassed: %d", w.Code)
	}
}

func TestLFSWriteAuthorizationRechecksPAT(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id"}
	if err := s.bindUser(t.Context(), "alice", identity); err != nil {
		t.Fatal(err)
	}
	raw, metadata, err := s.createToken(t.Context(), "alice", identity, "LFS", "write", []string{"alice/project"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	s.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		authorize := gittransport.WriteAuthorization(r.Context())
		if authorize == nil || authorize(r.Context()) != nil {
			t.Fatal("write authorization callback missing or failed")
		}
		if err := s.revokeToken(r.Context(), "alice", metadata.ID); err != nil {
			t.Fatal(err)
		}
		if err := authorize(r.Context()); err == nil {
			t.Fatal("revoked PAT accepted at upload publication")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r := httptest.NewRequestWithContext(t.Context(), "PUT", "/alice/project.git/info/lfs/objects/"+strings.Repeat("a", 64), strings.NewReader("payload"))
	r.SetBasicAuth("alice", raw)
	s.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("authorized upload did not reach handler")
	}
}

func lfsGrantFixture(t *testing.T) (*Service, SSHPrincipal, []byte, string) {
	t.Helper()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	principal := SSHPrincipal{Username: "alice", Identity: Identity{Subject: "alice-id"}}
	if err := s.bindUser(t.Context(), "alice", principal.Identity); err != nil {
		t.Fatal(err)
	}
	key := testSSHPublicKey(t, 42)
	record, err := s.createSSHKey(t.Context(), principal, "LFS SSH", sshAuthorizedKey(key))
	if err != nil {
		t.Fatal(err)
	}
	return s, principal, key.Marshal(), record.ID
}

func TestLFSGrantScopeExpiryAndRevocation(t *testing.T) {
	t.Parallel()
	s, principal, key, keyID := lfsGrantFixture(t)
	credentials, err := s.IssueLFSCredentials(t.Context(), principal, key, "alice", "project", "download")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Href != "https://git.example/alice/project.git/info/lfs" || credentials.ExpiresIn != 900 {
		t.Fatalf("unexpected SSH response: %+v", credentials)
	}
	header := credentials.Header["Authorization"]
	for _, test := range []struct {
		name, method, path, body string
		edit                     func(*http.Request)
		status                   int
	}{
		{name: "download", method: "GET", path: "/alice/project.git/info/lfs/objects/" + strings.Repeat("a", 64), status: 204},
		{name: "download batch", method: "POST", path: "/alice/project.git/info/lfs/objects/batch", body: `{"operation":"download"}`, status: 204},
		{name: "cannot upload", method: "POST", path: "/alice/project.git/info/lfs/objects/batch", body: `{"operation":"upload"}`, status: 403},
		{name: "cannot access another repo", method: "GET", path: "/alice/other.git/info/lfs/objects/" + strings.Repeat("a", 64), status: 403},
		{name: "cannot access Git", method: "GET", path: "/alice/project.git/info/refs?service=git-upload-pack", status: 401},
		{name: "duplicate authorization", method: "GET", path: "/alice/project.git/info/lfs/objects/" + strings.Repeat("a", 64), edit: func(r *http.Request) { r.Header.Add("Authorization", header) }, status: 401},
		{name: "foreign origin", method: "GET", path: "/alice/project.git/info/lfs/objects/" + strings.Repeat("a", 64), edit: func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, status: 403},
		{name: "tampered grant", method: "GET", path: "/alice/project.git/info/lfs/objects/" + strings.Repeat("a", 64), edit: func(r *http.Request) { r.Header.Set("Authorization", header+"x") }, status: 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), test.method, test.path, strings.NewReader(test.body))
			r.Header.Set("Authorization", header)
			if test.edit != nil {
				test.edit(r)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.status, w.Body.String())
			}
		})
	}
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/alice/project.git/info/lfs/objects/"+strings.Repeat("a", 64), nil)
	r.Header.Set("Authorization", header)
	grant, err := s.readLFSGrant(r)
	if err != nil {
		t.Fatal(err)
	}
	grant.Expires = time.Now().Add(-time.Second).Unix()
	expired, err := s.sessionCodec.Encode(lfsGrantLabel, grant)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+lfsGrantPrefix+expired)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired grant accepted: %d", w.Code)
	}
	if err := s.revokeSSHKey(t.Context(), "alice", keyID); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", header)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked SSH key accepted: %d", w.Code)
	}
}

func TestLFSGrantCrossShardAuthorityAndRevocation(t *testing.T) {
	t.Parallel()
	user, principal, key, keyID := lfsGrantFixture(t)
	group := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	cookie, csrf := groupSession(t, group, "alice", "alice-id")
	created := groupRequest(group, "POST", "/acme/", "", cookie, csrf)
	if created.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", created.Code, created.Body.String())
	}
	destination, _ := url.Parse("http://user-shard.internal")
	group.tokenResolver = tokenTestResolver{destination}
	calls := 0
	group.tokenClient.Transport = tokenTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "user-shard.internal" || r.URL.Path != "/api/v1/users/alice/lfs/verify" {
			t.Fatal("grant sent to unexpected authority")
		}
		w := httptest.NewRecorder()
		user.ServeHTTP(w, r)
		return w.Result(), nil
	})
	credentials, err := group.IssueLFSCredentials(t.Context(), principal, key, "acme", "project", "upload")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	group.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		authorize := gittransport.WriteAuthorization(r.Context())
		if authorize == nil || authorize(r.Context()) != nil {
			t.Fatal("cross-shard LFS authorization failed")
		}
		if err := user.revokeSSHKey(r.Context(), "alice", keyID); err != nil {
			t.Fatal(err)
		}
		if authorize(r.Context()) == nil {
			t.Fatal("SSH revocation did not prevent upload publication")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r := httptest.NewRequestWithContext(t.Context(), "PUT", "/acme/project.git/info/lfs/objects/"+strings.Repeat("a", 64), strings.NewReader("payload"))
	r.Header.Set("Authorization", credentials.Header["Authorization"])
	w := httptest.NewRecorder()
	group.ServeHTTP(w, r)
	if !called || w.Code != http.StatusNoContent || calls != 3 {
		t.Fatalf("handler=%v status=%d authority calls=%d", called, w.Code, calls)
	}
	// A signed grant is never enough when its authority redirects or fails.
	group.tokenClient.Transport = tokenTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://other.example"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	w = httptest.NewRecorder()
	group.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authority redirect accepted: %d", w.Code)
	}
}

func TestLFSGrantRechecksGroupMembership(t *testing.T) {
	t.Parallel()
	s, principal, key, _ := lfsGrantFixture(t)
	// The one-shard router keeps the key authority and group in this fixture.
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	s.router, err = shard.NewRouter(1, parser)
	if err != nil {
		t.Fatal(err)
	}
	s.local = 0
	record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:owner",
		Members: map[string]string{"google:owner": "owner", "google:alice-id": "developer"}}
	if err := s.writeNamespace(t.Context(), "acme", record, ""); err != nil {
		t.Fatal(err)
	}
	credentials, err := s.IssueLFSCredentials(t.Context(), principal, key, "acme", "project", "upload")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	s.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		authorize := gittransport.WriteAuthorization(r.Context())
		if authorize == nil {
			t.Fatal("missing LFS authorization callback")
		}
		if _, err := s.updateGroup(r.Context(), "acme", "google:owner", "set-role", "google:alice-id", "reader"); err != nil {
			t.Fatal(err)
		}
		if authorize(r.Context()) == nil {
			t.Fatal("membership downgrade did not prevent publication")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r := httptest.NewRequestWithContext(t.Context(), "PUT", "/acme/project.git/info/lfs/objects/"+strings.Repeat("a", 64), strings.NewReader("payload"))
	r.Header.Set("Authorization", credentials.Header["Authorization"])
	s.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("authorized upload did not reach handler")
	}
}

func TestLFSBatchBodyPreserved(t *testing.T) {
	t.Parallel()
	body := `{"operation":"download","objects":[{"oid":"abc","size":123}],"ref":{"name":"refs/heads/main"}}`
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/alice/repo.git/info/lfs/objects/batch", strings.NewReader(body))
	write, err := lfsWriteRequest(r, []string{"objects", "batch"})
	if err != nil || write {
		t.Fatalf("classification: write=%v err=%v", write, err)
	}
	var value map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil || len(value) != 3 {
		t.Fatalf("handler cannot read original batch: %v", err)
	}
}

func TestLFSGrantExpiryOnlyControlsAdmission(t *testing.T) {
	t.Parallel()
	for _, remote := range []bool{false, true} {
		name := "local key authority"
		if remote {
			name = "remote key authority"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				user, principal, key, _ := lfsGrantFixture(t)
				service, namespace := user, "alice"
				if remote {
					service, namespace = testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil), "acme"
					record := namespaceRecord{SchemaVersion: 1, Type: groupNamespace, CreatorUserID: "google:alice-id",
						Members: map[string]string{"google:alice-id": "owner"}}
					if err := service.writeNamespace(t.Context(), namespace, record, ""); err != nil {
						t.Fatal(err)
					}
					destination, _ := url.Parse("http://user-shard.internal")
					service.tokenResolver = tokenTestResolver{destination}
					service.tokenClient.Transport = tokenTestTransport(func(r *http.Request) (*http.Response, error) {
						if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+lfsKeyCheckPrefix) {
							t.Fatal("authority request did not use a separate server credential")
						}
						w := httptest.NewRecorder()
						user.ServeHTTP(w, r)
						return w.Result(), nil
					})
				}
				credentials, err := service.IssueLFSCredentials(t.Context(), principal, key, namespace, "project", "upload")
				if err != nil {
					t.Fatal(err)
				}
				completed := false
				service.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
					defer cancel()
					time.Sleep(lfsGrantLifetime + time.Second)
					authorize := gittransport.WriteAuthorization(ctx)
					if authorize == nil || authorize(ctx) != nil {
						t.Fatal("admitted transfer was rejected solely because its grant expired")
					}
					completed = true
					w.WriteHeader(http.StatusNoContent)
				})
				path := "/" + namespace + "/project.git/info/lfs/objects/" + strings.Repeat("a", 64)
				request := func() *httptest.ResponseRecorder {
					r := httptest.NewRequestWithContext(t.Context(), "PUT", path, strings.NewReader("payload"))
					r.Header.Set("Authorization", credentials.Header["Authorization"])
					w := httptest.NewRecorder()
					service.ServeHTTP(w, r)
					return w
				}
				if w := request(); w.Code != http.StatusNoContent || !completed {
					t.Fatalf("admitted upload failed: %d %s", w.Code, w.Body.String())
				}
				if w := request(); w.Code != http.StatusUnauthorized {
					t.Fatalf("expired grant admitted a new request: %d", w.Code)
				}
			})
		})
	}
}

func TestLFSBatchAuthenticationAdmission(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id"}
	if err := s.bindUser(t.Context(), "alice", identity); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.createToken(t.Context(), "alice", identity, "LFS", "read", []string{"alice/project"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	admitted, release := make(chan struct{}, 8), make(chan struct{})
	var workers sync.WaitGroup
	defer workers.Wait()
	defer close(release)
	s.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		admitted <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	for range 8 {
		workers.Go(func() {
			r := httptest.NewRequestWithContext(t.Context(), "POST", "/alice/project.git/info/lfs/objects/batch", strings.NewReader(`{"operation":"download","objects":[]}`))
			r.SetBasicAuth("alice", token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("admitted batch status=%d", w.Code)
			}
		})
	}
	for range 8 {
		select {
		case <-admitted:
		case <-time.After(5 * time.Second):
			t.Fatal("batch fixture did not reach the handler")
		}
	}
	// The malformed ninth body would produce 400 if parsed. Admission must
	// reject it before allocating or reading another control body.
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/alice/project.git/info/lfs/objects/batch", strings.NewReader("invalid"))
	r.SetBasicAuth("alice", token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("excess batch was not rejected before parsing: %d", w.Code)
	}
}
