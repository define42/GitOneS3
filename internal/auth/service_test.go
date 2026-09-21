package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

type fakeProvider struct {
	calls    atomic.Int32
	identity Identity
	fail     bool
}

func (p *fakeProvider) AuthorizationURL(state, nonce, verifier string) string {
	cfg := oauth2.Config{ClientID: "client", RedirectURL: "https://git.example" + CallbackPath,
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/auth"}, Scopes: []string{"openid", "email"}}
	return cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.S256ChallengeOption(verifier))
}

func (p *fakeProvider) Exchange(_ context.Context, code, verifier, nonce string) (Identity, error) {
	p.calls.Add(1)
	if p.fail || code != "valid-code" || verifier == "" || nonce == "" {
		return Identity{}, errors.New("invalid exchange")
	}
	return p.identity, nil
}

func testConfig() config.Auth {
	return config.Auth{Enabled: true, PublicURL: "https://git.example", GoogleClientID: "client", GoogleClientSecret: "test-client-secret",
		CookieHashKey:  base64.StdEncoding.EncodeToString([]byte(strings.Repeat("h", 64))),
		CookieBlockKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("e", 32)))}
}

func testService(t *testing.T, local shard.ShardID, store storage.ObjectStore, provider Provider, next http.Handler) *Service {
	t.Helper()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(2, parser)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject, ok := Subject(r.Context())
			if !ok || !subject.Authenticated || subject.UserID == "" {
				t.Error("missing authenticated subject")
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	s, err := New(Options{Config: testConfig(), LocalShard: local, Router: router, Store: store, Provider: provider, Next: next})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startLogin(t *testing.T, handler http.Handler) (string, *http.Cookie) {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/alice/auth/google/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login status = %d: %s", w.Code, w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("nonce") == "" || location.Query().Get("code_challenge_method") != "S256" ||
		location.Query().Get("redirect_uri") != "https://git.example"+CallbackPath {
		t.Fatal("missing OIDC protections")
	}
	cookie := responseCookie(t, w, loginCookie)
	return location.Query().Get("state"), cookie
}

func responseCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == name {
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
				t.Fatalf("insecure cookie attributes: %+v", cookie)
			}
			return cookie
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}

func callbackRequest(state string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, CallbackPath+"?"+url.Values{"code": {"valid-code"}, "state": {state}}.Encode(), nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type resolverFunc func(shard.ShardID) (*url.URL, error)

func (f resolverFunc) Resolve(id shard.ShardID) (*url.URL, error) { return f(id) }

func TestLoginCallbackAndSessionAcrossShardsAndRestart(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{identity: Identity{Subject: "google-alice", Email: "alice@example.com"}}
	store := storage.NewMemoryStore()
	owner := testService(t, 1, store, provider, nil)
	entryProvider := &fakeProvider{fail: true}
	entryStore := storage.NewMemoryStore()
	entry := testService(t, 0, entryStore, entryProvider, nil)
	var forwards int
	handler, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard: 0, Router: entry, Next: entry,
		Resolver: resolverFunc(func(id shard.ShardID) (*url.URL, error) {
			if id != 1 {
				t.Fatalf("forwarded to shard %d", id)
			}
			return url.Parse("http://gitone-1.internal:8080")
		}),
		Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			forwards++
			w := httptest.NewRecorder()
			owner.ServeHTTP(w, r)
			return w.Result(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, browser := startLogin(t, handler)
	// Restart the owner before the callback: state and PKCE must be durable.
	owner = testService(t, 1, store, provider, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, callbackRequest(state, browser))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/alice" {
		t.Fatalf("callback = %d: %s", w.Code, w.Body.String())
	}
	cookie := responseCookie(t, w, sessionCookie)
	if forwards != 2 || provider.calls.Load() != 1 || entryProvider.calls.Load() != 0 {
		t.Fatal("exchange did not run exclusively on owner")
	}
	objects, err := entryStore.List(context.Background(), "auth/")
	if err != nil || len(objects) != 0 {
		t.Fatal("entry shard touched auth storage")
	}
	for _, path := range []string{"/alice", "/alice/", "/alice/auth/session", "/alice/repo.git/info/refs"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
			t.Fatalf("session %s: %d", path, w.Code)
		}
	}
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, callbackRequest(state, browser))
	if replay.Code != http.StatusBadRequest || provider.calls.Load() != 1 {
		t.Fatal("callback replay succeeded")
	}
}

func TestCallbackRejectsInvalidStateBeforeExchange(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "tampered", "expired", "wrong owner", "wrong origin", "invalid username", "duplicate", "wrong key"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProvider{identity: Identity{Subject: "alice"}}
			s := testService(t, 1, storage.NewMemoryStore(), p, nil)
			encoded, browser := startLogin(t, s)
			var state loginState
			if err := s.loginCodec.Decode(stateLabel, encoded, &state); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "expired":
				state.Expires = time.Now().Add(-time.Minute).Unix()
			case "wrong owner":
				state.Owner = 0
			case "wrong origin":
				state.Origin = "https://other.example"
			case "invalid username":
				state.Username = "../alice"
			}
			if name == "wrong key" {
				cfg := testConfig()
				cfg.CookieHashKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
				other, err := New(Options{Config: cfg, LocalShard: 1, Router: s.router, Store: s.store, Provider: p, Next: s.next})
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ = other.loginCodec.Encode(stateLabel, state)
			} else {
				encoded, _ = s.loginCodec.Encode(stateLabel, state)
			}
			if name == "missing" {
				encoded = ""
			}
			if name == "tampered" {
				encoded = "x" + encoded
			}
			r := callbackRequest(encoded, browser)
			if name == "duplicate" {
				r.URL.RawQuery += "&state=" + url.QueryEscape(encoded)
			}
			if _, err := s.Resolve(r); err == nil {
				t.Fatal("invalid state accepted for routing")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || p.calls.Load() != 0 {
				t.Fatal("invalid state reached token exchange")
			}
		})
	}
}

func TestCallbackBrowserBindingAndErrors(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing cookie", "wrong browser", "duplicate cookie", "denied", "exchange failure", "duplicate code"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProvider{identity: Identity{Subject: "alice"}, fail: name == "exchange failure"}
			s := testService(t, 1, storage.NewMemoryStore(), p, nil)
			state, browser := startLogin(t, s)
			if name == "missing cookie" {
				browser = nil
			}
			if name == "wrong browser" {
				browser.Value = "wrong-browser"
			}
			r := callbackRequest(state, browser)
			if name == "duplicate cookie" {
				r.AddCookie(browser)
			}
			if name == "denied" {
				r.URL.RawQuery = url.Values{"state": {state}, "error": {"access_denied"}}.Encode()
			}
			if name == "duplicate code" {
				r.URL.RawQuery += "&code=other"
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Fatalf("invalid callback status = %d", w.Code)
			}
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == sessionCookie && cookie.Value != "" {
					t.Fatal("invalid callback created session")
				}
			}
			if name != "exchange failure" && p.calls.Load() != 0 {
				t.Fatal("invalid callback reached exchange")
			}
		})
	}
}

func TestConcurrentCallbackIsSingleUse(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{identity: Identity{Subject: "alice"}}
	s := testService(t, 1, storage.NewMemoryStore(), p, nil)
	state, browser := startLogin(t, s)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.ServeHTTP(w, callbackRequest(state, browser))
			if w.Code == http.StatusSeeOther {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || p.calls.Load() != 1 {
		t.Fatalf("successes=%d exchanges=%d", successes.Load(), p.calls.Load())
	}
}

func TestSessionIsolationExpiryAndCSRF(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{identity: Identity{Subject: "alice"}}
	s := testService(t, 1, storage.NewMemoryStore(), p, nil)
	state, browser := startLogin(t, s)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, callbackRequest(state, browser))
	cookie := responseCookie(t, w, sessionCookie)
	var current session
	if err := s.sessionCodec.Decode(sessionCookie, cookie.Value, &current); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path, method, origin, csrf string
		status                           int
	}{
		{"anonymous", "/alice", "GET", "", "", 401},
		{"tampered", "/alice", "GET", "", "", 401},
		{"expired", "/alice", "GET", "", "", 401},
		{"different namespace", "/acme", "GET", "", "", 403},
		{"missing csrf", "/alice/repo.git/git-receive-pack", "POST", s.origin, "", 403},
		{"wrong origin", "/alice/repo.git/git-receive-pack", "POST", "https://evil.example", current.CSRF, 403},
		{"valid write", "/alice/repo.git/git-receive-pack", "POST", s.origin, current.CSRF, 204},
		{"logout", "/alice/auth/logout", "POST", s.origin, current.CSRF, 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.path, nil)
			c := *cookie
			if test.name == "tampered" {
				c.Value = "x" + c.Value
			}
			if test.name == "expired" {
				expired := current
				expired.Expires = time.Now().Add(-time.Minute).Unix()
				c.Value, _ = s.sessionCodec.Encode(sessionCookie, expired)
			}
			if test.name != "anonymous" {
				r.AddCookie(&c)
			}
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-CSRF-Token", test.csrf)
			handler := s
			if test.name == "different namespace" {
				handler = testService(t, 0, storage.NewMemoryStore(), p, nil)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d", w.Code, test.status)
			}
			if test.name == "logout" && responseCookie(t, w, sessionCookie).MaxAge != -1 {
				t.Fatal("logout did not clear session")
			}
		})
	}
}
