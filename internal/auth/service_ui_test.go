package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestUILoginModes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, mode string
		existing   bool
		wantError  string
	}{
		{name: "register new", mode: "register"},
		{name: "login existing", mode: "login", existing: true},
		{name: "register occupied", mode: "register", existing: true, wantError: "already claimed"},
		{name: "login missing", mode: "login", wantError: "not registered"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := &fakeProvider{identity: Identity{Subject: "alice", Email: "alice@example.com"}}
			s := testService(t, 1, storage.NewMemoryStore(), provider, nil)
			if tt.existing {
				if err := s.bindUser(context.Background(), "alice", provider.identity); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/alice/auth/oidc/login?mode="+tt.mode+"&ui=1&returnTo=%2Fteam%2Finvitations%2Faccept",
				nil,
			)
			response := httptest.NewRecorder()
			s.ServeHTTP(response, request)
			location, err := url.Parse(response.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" {
				if response.Code != http.StatusSeeOther || !strings.Contains(location.Query().Get("error"), tt.wantError) {
					t.Fatalf("response = %d %s", response.Code, location)
				}
				return
			}
			if response.Code != http.StatusFound {
				t.Fatalf("login = %d: %s", response.Code, response.Body.String())
			}
			callback := callbackRequest(location.Query().Get("state"), responseCookie(t, response, loginCookie))
			completed := httptest.NewRecorder()
			s.ServeHTTP(completed, callback)
			if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/team/invitations/accept" {
				t.Fatalf("callback = %d %s: %s", completed.Code, completed.Header().Get("Location"), completed.Body.String())
			}
			responseCookie(t, completed, sessionCookie)
		})
	}
}

func TestUIRegistrationRemainsAtomic(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{identity: Identity{Subject: "alice"}}
	s := testService(t, 1, storage.NewMemoryStore(), provider, nil)
	login := httptest.NewRecorder()
	s.ServeHTTP(login, httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/alice/auth/oidc/login?mode=register&ui=1",
		nil,
	))
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// Even the same identity claiming the name in another browser must make
	// this explicit registration fail, not silently become a sign-in.
	if err := s.bindUser(context.Background(), "alice", provider.identity); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, callbackRequest(location.Query().Get("state"), responseCookie(t, login, loginCookie)))
	redirect, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusSeeOther || redirect.Path != "/auth/register" ||
		redirect.Query().Get("error") != "username is already claimed" {
		t.Fatalf("callback = %d %s", response.Code, redirect)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			t.Fatal("registration race issued a session")
		}
	}
}

func TestUIRejectsUnsafeReturnPaths(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, target := range []string{"https://evil.example/", "//evil.example/", "/%2fevil", "/alice/../bob", "/alice?next=x", "/alice#fragment", "/alice/auth/oidc/login", "/auth/oidc/callback", "/api/v1/session", "/alice\\evil"} {
		t.Run(target, func(t *testing.T) {
			response := httptest.NewRecorder()
			s.ServeHTTP(response, httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/alice/auth/oidc/login?returnTo="+url.QueryEscape(target),
				nil,
			))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("returnTo %q = %d", target, response.Code)
			}
		})
	}
}

func TestUIAcceptsCanonicalReturnPaths(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, target := range []string{"/", "/auth/new-group", "/alice", "/alice/", "/team/settings", "/team/settings/", "/team/invitations/accept/"} {
		t.Run(target, func(t *testing.T) {
			response := httptest.NewRecorder()
			s.ServeHTTP(response, httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/alice/auth/oidc/login?returnTo="+url.QueryEscape(target),
				nil,
			))
			if response.Code != http.StatusFound {
				t.Fatalf("returnTo %q = %d", target, response.Code)
			}
		})
	}
}

func TestUIRepositoryReturnPathCodecBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		target string
		want   int
	}{
		{"near limit", "/alice/project?path=" + strings.Repeat("x", 980), http.StatusFound},
		{"raw bytes over limit", "/alice/project?path=" + strings.Repeat("x", 3000), http.StatusBadRequest},
		{"json escaping over limit", "/alice/project?path=" + strings.Repeat("<", 400), http.StatusBadRequest},
		{"encoded unicode", "/alice/project?path=" + strings.Repeat("%C3%A6", 100), http.StatusFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &fakeProvider{identity: Identity{Subject: "alice"}}
			s := testService(t, 1, storage.NewMemoryStore(), provider, nil)
			response := httptest.NewRecorder()
			s.ServeHTTP(response, httptest.NewRequestWithContext(
				t.Context(),
				"GET",
				"/alice/auth/oidc/login?mode=register&ui=1&returnTo="+url.QueryEscape(test.target),
				nil,
			))
			if response.Code != test.want {
				t.Fatalf("login=%d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.want != http.StatusFound {
				return
			}
			location, err := url.Parse(response.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			callback := httptest.NewRecorder()
			s.ServeHTTP(callback, callbackRequest(location.Query().Get("state"), responseCookie(t, response, loginCookie)))
			if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != test.target {
				t.Fatalf("callback=%d location=%q", callback.Code, callback.Header().Get("Location"))
			}
		})
	}
}

func TestUICallbackRejectsWrongAccount(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{identity: Identity{Subject: "other"}}, nil)
	if err := s.bindUser(context.Background(), "alice", Identity{Subject: "alice"}); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	s.ServeHTTP(login, httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/alice/auth/oidc/login?mode=login&ui=1",
		nil,
	))
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, callbackRequest(location.Query().Get("state"), responseCookie(t, login, loginCookie)))
	redirect, _ := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusSeeOther || redirect.Path != "/auth/login" || redirect.Query().Get("error") == "" {
		t.Fatalf("wrong account = %d %s", response.Code, redirect)
	}
	const wantError = "The selected provider account does not own this username. Try another account."
	if redirect.Query().Get("error") != wantError || redirect.Query().Get("username") != "alice" {
		t.Fatalf("wrong account must preserve username and explain retry: %s", redirect)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			t.Fatal("wrong account issued a session")
		}
	}
	record, _, err := s.loadNamespace(t.Context(), "alice")
	if err != nil || record.Subject != "alice" {
		t.Fatalf("wrong account changed namespace ownership: %+v, %v", record.Identity, err)
	}
}
