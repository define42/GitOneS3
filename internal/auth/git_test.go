package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestGitCredentialRules(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id"}
	if err := s.bindUser(context.Background(), "alice", identity); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.createToken(context.Background(), "alice", identity, "Laptop", "read", []string{"alice/project"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	for _, test := range []struct {
		name   string
		edit   func(*http.Request)
		status int
	}{
		{"anonymous", func(*http.Request) {}, 401},
		{"valid PAT", func(r *http.Request) { r.SetBasicAuth("alice", raw) }, 204},
		{"off scope", func(r *http.Request) {
			r.SetBasicAuth("alice", raw)
			r.URL.Path = "/alice/other.git/info/refs"
			r.RequestURI = "/alice/other.git/info/refs?service=git-upload-pack"
		}, 403},
		{"wrong username", func(r *http.Request) { r.SetBasicAuth("bob", raw) }, 401},
		{"unsupported auth", func(r *http.Request) { r.Header.Set("Authorization", "Bearer invalid") }, 401},
		{"no cookie fallback", func(r *http.Request) { r.AddCookie(cookie); r.Header.Set("Authorization", "Basic invalid") }, 401},
		{"duplicate headers", func(r *http.Request) {
			r.SetBasicAuth("alice", raw)
			r.Header.Add("Authorization", r.Header.Get("Authorization"))
		}, 401},
		{"oversized header", func(r *http.Request) { r.Header.Set("Authorization", strings.Repeat("x", 513)) }, 401},
		{"cross origin", func(r *http.Request) { r.SetBasicAuth("alice", raw); r.Header.Set("Origin", "https://evil.example") }, 403},
		{"read-only receive", func(r *http.Request) { r.SetBasicAuth("alice", raw); r.URL.RawQuery = "service=git-receive-pack" }, 403},
		{"cookie read compatibility", func(r *http.Request) { r.AddCookie(cookie) }, 204},
		{"cookie post needs CSRF", func(r *http.Request) {
			r.AddCookie(cookie)
			r.Method = "POST"
			r.URL.Path = "/alice/project.git/git-upload-pack"
			r.RequestURI = r.URL.Path
			r.URL.RawQuery = ""
		}, 403},
		{"cookie post CSRF", func(r *http.Request) {
			r.AddCookie(cookie)
			r.Method = "POST"
			r.URL.Path = "/alice/project.git/git-upload-pack"
			r.RequestURI = r.URL.Path
			r.URL.RawQuery = ""
			r.Header.Set("Origin", s.origin)
			r.Header.Set("X-CSRF-Token", csrf)
		}, 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(
				t.Context(),
				"GET",
				"/alice/project.git/info/refs?service=git-upload-pack",
				nil,
			)
			test.edit(r)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d want=%d", w.Code, test.status)
			}
			if w.Code == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing Git Basic challenge")
			}
		})
	}
}

type tokenTestResolver struct{ destination *url.URL }

func (r tokenTestResolver) Resolve(shard.ShardID) (*url.URL, error) { return r.destination, nil }

type tokenTestTransport func(*http.Request) (*http.Response, error)

func (f tokenTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTokenAuthorityDoesNotFollowRedirectsOrFailOpen(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	authority := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	raw, _, err := authority.createToken(context.Background(), "alice", Identity{Subject: "alice-id"}, "Laptop", "read", []string{"acme/project"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	destination, _ := url.Parse("http://authority.example")
	s.tokenResolver = tokenTestResolver{destination}
	for _, status := range []int{302, 401, 403, 503, 200} {
		calls := 0
		s.tokenClient.Transport = tokenTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "authority.example" || r.URL.Path != "/api/v1/users/alice/tokens/verify" {
				t.Fatal("credential sent to unexpected authority")
			}
			w := httptest.NewRecorder()
			w.Header().Set("Location", "http://other.example")
			w.WriteHeader(status)
			_, _ = w.WriteString(`{"username":"alice"}`)
			return w.Result(), nil
		})
		if _, err := s.verifyToken(context.Background(), tokenCredentials{"alice", raw}); err == nil {
			t.Fatal("invalid authority response accepted")
		}
		if calls != 1 {
			t.Fatal("redirect followed")
		}
	}
	s.tokenClient.Transport = tokenTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	if _, err := s.verifyToken(context.Background(), tokenCredentials{"alice", raw}); err == nil {
		t.Fatal("authority outage accepted")
	}
}

func TestTokenAuthorityRepositorySelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	authority := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	raw, view, err := authority.createToken(ctx, "alice", Identity{Subject: "alice-id"}, "Laptop", "read", nil, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	destination, _ := url.Parse("http://authority.example")
	s.tokenResolver = tokenTestResolver{destination}
	for _, test := range []struct {
		name   string
		all    bool
		scopes []string
		valid  bool
	}{
		{name: "explicit all scope", all: true, scopes: []string{}, valid: true},
		{name: "legacy exact scope", scopes: []string{"acme/project"}, valid: true},
		{name: "missing flag and empty list rejected", scopes: []string{}},
		{name: "mixed scope rejected", all: true, scopes: []string{"acme/project"}},
		{name: "wildcard never inferred", scopes: []string{"*"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			principal := tokenPrincipal{Username: "alice", TokenID: view.ID, Identity: Identity{Subject: "alice-id"}, Permission: "read", AllRepositories: test.all, Repositories: test.scopes, ExpiresAt: view.ExpiresAt}
			s.tokenClient.Transport = tokenTestTransport(func(*http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(principal); err != nil {
					t.Fatal(err)
				}
				return w.Result(), nil
			})
			got, err := s.verifyToken(ctx, tokenCredentials{"alice", raw})
			if (err == nil) != test.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, test.valid)
			}
			if err == nil && got.AllRepositories != test.all {
				t.Fatal("authority scope was not preserved")
			}
		})
	}
}
