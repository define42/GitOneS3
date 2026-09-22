package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestResolveAPI(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, test := range []struct {
		name  string
		path  string
		owner shard.ShardID
		bad   bool
	}{
		{"session on entry shard", "/api/v1/session", 0, false},
		{"docs on entry shard", "/api/docs", 0, false},
		{"name owner", "/api/v1/names/alice", 1, false},
		{"user owner", "/api/v1/users/alice", 1, false},
		{"group owner", "/api/v1/groups/acme", 0, false},
		{"group members owner", "/api/v1/groups/acme/members", 0, false},
		{"space shard", "/api/v1/spaces?shard=1", 1, false},
		{"missing shard", "/api/v1/spaces", 0, true},
		{"duplicate shard", "/api/v1/spaces?shard=0&shard=1", 0, true},
		{"out of range shard", "/api/v1/spaces?shard=2", 0, true},
		{"negative shard", "/api/v1/spaces?shard=-1", 0, true},
		{"noncanonical shard", "/api/v1/spaces?shard=01", 0, true},
		{"invalid namespace", "/api/v1/groups/ALICE", 0, true},
		{"reserved namespace", "/api/v1/groups/api", 0, true},
		{"auth namespace", "/api/v1/groups/auth", 0, true},
		{"encoded namespace", "/api/v1/groups/%61lice", 0, true},
		{"encoded separator", "/api/v1/groups/alice%2Fmembers", 0, true},
		{"empty component", "/api/v1/groups//members", 0, true},
		{"dot component", "/api/v1/groups/alice/../acme", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			route, err := s.resolveAPI(httptest.NewRequest("GET", test.path, nil))
			if test.bad {
				if err == nil {
					t.Fatal("invalid routing input accepted")
				}
				return
			}
			if err != nil || route.Owner != test.owner {
				t.Fatalf("route=%+v err=%v; want owner %d", route, err, test.owner)
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"encoded request target without raw path", func(r *http.Request) { r.RequestURI = "/api/v1/names/%61lice" }},
		{"different request target", func(r *http.Request) { r.RequestURI = "/api/v1/names/acme" }},
		{"absolute request target", func(r *http.Request) { r.RequestURI = "https://git.example/api/v1/names/alice" }},
		{"opaque url", func(r *http.Request) { r.URL.Opaque = "//git.example/api/v1/names/alice" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("GET", "/api/v1/names/alice", nil)
			test.change(r)
			if _, err := s.resolveAPI(r); err == nil {
				t.Fatal("ambiguous request accepted")
			}
		})
	}
}

func TestAPISessionAndLogout(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	handler := s.newAPIHandler()
	for _, test := range []struct {
		name          string
		cookie        *http.Cookie
		authenticated bool
	}{
		{"anonymous", nil, false},
		{"invalid cookie", &http.Cookie{Name: sessionCookie, Value: "invalid"}, false},
		{"signed session", func() *http.Cookie { cookie, _ := groupSession(t, s, "alice", "alice-id"); return cookie }(), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			w := groupRequest(handler, "GET", "/api/v1/session", "", test.cookie, "")
			if w.Code != http.StatusOK {
				t.Fatalf("session status=%d: %s", w.Code, w.Body.String())
			}
			var view sessionView
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Authenticated != test.authenticated || view.ShardCount != 2 || view.Provider != "google" {
				t.Fatalf("invalid session view: %+v", view)
			}
			if test.authenticated && (view.UserID != "google:alice-id" || view.CSRF == "" || view.Username != "alice") {
				t.Fatalf("missing authenticated details: %+v", view)
			}
			if !test.authenticated && (view.Identity != nil || view.CSRF != "" || view.Username != "") {
				t.Fatalf("anonymous response disclosed identity: %+v", view)
			}
		})
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	w := groupRequest(handler, "POST", "/api/v1/logout", "", cookie, csrf)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("logout=%d: %s", w.Code, w.Body.String())
	}
	cleared := responseCookie(t, w, sessionCookie)
	if cleared.Value != "" || cleared.MaxAge != -1 || !cleared.Secure || !cleared.HttpOnly || cleared.Path != "/" {
		t.Fatalf("insecure logout cookie: %+v", cleared)
	}
}

func TestAPIAvailabilityAndUserLookup(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	handler := s.newAPIHandler()
	checkAvailability := func(want bool) {
		t.Helper()
		w := groupRequest(handler, "GET", "/api/v1/names/alice", "", nil, "")
		var output struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || output.Name != "alice" || output.Available != want {
			t.Fatalf("availability status=%d response=%+v", w.Code, output)
		}
	}
	checkAvailability(true)
	if err := s.bindUser(context.Background(), "alice", Identity{Subject: "alice-id", Email: "private@example.com"}); err != nil {
		t.Fatal(err)
	}
	checkAvailability(false)
	if w := groupRequest(handler, "GET", "/api/v1/users/alice", "", nil, ""); w.Code != 401 {
		t.Fatalf("anonymous user lookup status=%d", w.Code)
	}
	cookie, csrf := groupSession(t, s, "bob", "bob-id")
	w := groupRequest(handler, "GET", "/api/v1/users/alice", "", cookie, csrf)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"userId":"google:alice-id"`) || strings.Contains(w.Body.String(), "private@example.com") {
		t.Fatalf("invalid user lookup status=%d: %s", w.Code, w.Body.String())
	}
	nameOwner, err := s.router.Owner("a--b")
	if err != nil {
		t.Fatal(err)
	}
	hyphenService := testService(t, nameOwner, storage.NewMemoryStore(), &fakeProvider{}, nil)
	w = groupRequest(hyphenService, "GET", "/api/v1/names/a--b", "", nil, "")
	if w.Code != 200 {
		t.Fatalf("valid repeated internal hyphen rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestAPIGroupLifecycleAndPrivateDiscovery(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	handler := s.newAPIHandler()
	owner, ownerCSRF := groupSession(t, s, "alice", "alice-id")
	member, memberCSRF := groupSession(t, s, "bob", "bob-id")
	outsider, outsiderCSRF := groupSession(t, s, "eve", "eve-id")
	check := func(method, path, body string, cookie *http.Cookie, csrf string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := groupRequest(handler, method, path, body, cookie, csrf)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	check("POST", "/api/v1/groups/acme", "", owner, ownerCSRF, 201)
	check("POST", "/api/v1/groups/acme", "", member, memberCSRF, 409)
	check("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:bob-id","role":"reader"}`, owner, ownerCSRF, 200)
	check("GET", "/api/v1/groups/acme", "", member, memberCSRF, 403)
	check("GET", "/api/v1/groups/acme/invitation", "", outsider, outsiderCSRF, 404)
	preview := check("GET", "/api/v1/groups/acme/invitation", "", member, memberCSRF, 200)
	if strings.Contains(preview.Body.String(), "alice-id") || strings.Contains(preview.Body.String(), "members") {
		t.Fatal("invitation preview disclosed membership")
	}
	listed := check("GET", "/api/v1/spaces?shard=0", "", member, memberCSRF, 200)
	if !strings.Contains(listed.Body.String(), `"invited":true`) || !strings.Contains(listed.Body.String(), `"name":"acme"`) {
		t.Fatalf("missing invitation: %s", listed.Body.String())
	}
	listed = check("GET", "/api/v1/spaces?shard=0", "", outsider, outsiderCSRF, 200)
	if strings.Contains(listed.Body.String(), "acme") || !strings.Contains(listed.Body.String(), `"spaces":[]`) {
		t.Fatalf("outsider discovered group: %s", listed.Body.String())
	}
	check("POST", "/api/v1/groups/acme/invitations/accept", "", member, memberCSRF, 200)
	check("POST", "/api/v1/groups/acme/invitations/accept", "", member, memberCSRF, 404)
	listed = check("GET", "/api/v1/spaces?shard=0", "", member, memberCSRF, 200)
	if !strings.Contains(listed.Body.String(), `"invited":false`) {
		t.Fatalf("accepted invitation still listed as pending: %s", listed.Body.String())
	}
	check("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:eve-id","role":"developer"}`, owner, ownerCSRF, 200)
	view := check("GET", "/api/v1/groups/acme", "", member, memberCSRF, 200)
	if strings.Contains(view.Body.String(), "eve-id") || strings.Contains(view.Body.String(), "invitations") {
		t.Fatal("reader can see pending invitations")
	}
	check("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:eve-id","role":"owner"}`, member, memberCSRF, 403)
	check("PUT", "/api/v1/groups/acme/members", `{"userId":"google:bob-id","role":"owner"}`, member, memberCSRF, 403)
	check("DELETE", "/api/v1/groups/acme/members", `{"userId":"google:alice-id"}`, owner, ownerCSRF, 409)
	check("PUT", "/api/v1/groups/acme/members", `{"userId":"google:alice-id","role":"reader"}`, owner, ownerCSRF, 409)
	check("PUT", "/api/v1/groups/acme/members", `{"userId":"google:bob-id","role":"owner"}`, owner, ownerCSRF, 200)
	removed := check("DELETE", "/api/v1/groups/acme/members", `{"userId":"google:alice-id"}`, owner, ownerCSRF, 204)
	if removed.Body.Len() != 0 {
		t.Fatalf("removed owner receives roster: %s", removed.Body.String())
	}
	check("DELETE", "/api/v1/groups/acme/invitations", `{"userId":"google:eve-id"}`, member, memberCSRF, 200)
	check("GET", "/api/v1/groups/acme/invitation", "", outsider, outsiderCSRF, 404)
	check("GET", "/api/v1/groups/acme", "", owner, ownerCSRF, 403)
}

func TestAPIRejectsUnsafeAndInvalidRequests(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	handler := s.newAPIHandler()
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	created := groupRequest(handler, "POST", "/api/v1/groups/acme", "", cookie, csrf)
	if created.Code != 201 {
		t.Fatalf("group setup: %s", created.Body.String())
	}
	for _, test := range []struct {
		name   string
		body   string
		cookie *http.Cookie
		csrf   string
		origin string
		want   int
	}{
		{"anonymous", `{"userId":"google:bob","role":"reader"}`, nil, csrf, s.origin, 401},
		{"missing csrf", `{"userId":"google:bob","role":"reader"}`, cookie, "", s.origin, 403},
		{"wrong csrf", `{"userId":"google:bob","role":"reader"}`, cookie, "bad", s.origin, 403},
		{"missing origin", `{"userId":"google:bob","role":"reader"}`, cookie, csrf, "", 403},
		{"foreign origin", `{"userId":"google:bob","role":"reader"}`, cookie, csrf, "https://evil.example", 403},
		{"invalid role", `{"userId":"google:bob","role":"admin"}`, cookie, csrf, s.origin, 422},
		{"missing user", `{"role":"reader"}`, cookie, csrf, s.origin, 422},
		{"unknown property", `{"userId":"google:bob","role":"reader","admin":true}`, cookie, csrf, s.origin, 422},
		{"trailing json", `{"userId":"google:bob","role":"reader"}{}`, cookie, csrf, s.origin, 400},
		{"oversized body", `{"userId":"` + strings.Repeat("b", 5000) + `","role":"reader"}`, cookie, csrf, s.origin, 413},
		{"invalid identity", `{"userId":"bob","role":"reader"}`, cookie, csrf, s.origin, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("POST", "/api/v1/groups/acme/invitations", strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-CSRF-Token", test.csrf)
			if test.cookie != nil {
				r.AddCookie(test.cookie)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.want || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("status=%d, content type=%s, want %d: %s", w.Code, w.Header().Get("Content-Type"), test.want, w.Body.String())
			}
		})
	}
}

func TestAPIOpenAPIAndCrossShardForwarding(t *testing.T) {
	t.Parallel()
	stores := []*storage.MemoryStore{storage.NewMemoryStore(), storage.NewMemoryStore()}
	services := []*Service{
		testService(t, 0, stores[0], &fakeProvider{}, nil),
		testService(t, 1, stores[1], &fakeProvider{}, nil),
	}
	// Use the same owner check as Service.ServeHTTP while keeping this test focused
	// on the API contract and its proxy resolver integration.
	handlers := make([]http.Handler, len(services))
	for i, service := range services {
		api := service.newAPIHandler()
		handlers[i] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, err := service.resolveAPI(r)
			if err != nil || route.Owner != service.local {
				http.Error(w, "incorrect owner", 502)
				return
			}
			api.ServeHTTP(w, r)
		})
	}
	entry, err := proxy.NewHandler(proxy.HandlerOptions{
		LocalShard: 1, Router: apiRouter{services[1]}, Next: handlers[1],
		Resolver: resolverFunc(func(id shard.ShardID) (*url.URL, error) {
			return url.Parse(fmt.Sprintf("http://gitone-%d:8080", id))
		}),
		Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handlers[0].ServeHTTP(w, r)
			return w.Result(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, services[1], "alice", "alice-id")
	w := groupRequest(entry, "POST", "/api/v1/groups/acme", "", cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("cross-shard create=%d: %s", w.Code, w.Body.String())
	}
	if _, err := stores[1].Head(context.Background(), namespaceKey("acme")); err == nil {
		t.Fatal("group was created on caller shard")
	}
	w = groupRequest(entry, "GET", "/api/v1/spaces?shard=0", "", cookie, csrf)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"acme"`) {
		t.Fatalf("cross-shard discovery=%d: %s", w.Code, w.Body.String())
	}
	w = groupRequest(entry, "GET", "/api/openapi.json", "", nil, "")
	var document struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || document.OpenAPI != "3.1.0" || len(document.Paths) != 10 {
		t.Fatalf("invalid API specification: status=%d, paths=%d, openapi=%s", w.Code, len(document.Paths), document.OpenAPI)
	}
	if !strings.Contains(string(document.Paths["/api/v1/groups/{name}/invitations"]), `"security"`) {
		t.Fatal("OpenAPI does not describe session authentication")
	}
}

type apiRouter struct{ *Service }

func (r apiRouter) Resolve(request *http.Request) (shard.Route, error) {
	return r.resolveAPI(request)
}

func TestAPIRoutesAcrossFourShards(t *testing.T) {
	t.Parallel()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(4, parser)
	if err != nil {
		t.Fatal(err)
	}
	services := make([]*Service, 4)
	names := make([]string, 4)
	for i := range services {
		services[i], err = New(Options{Config: testConfig(), LocalShard: shard.ShardID(i), Router: router,
			Store: storage.NewMemoryStore(), Provider: &fakeProvider{}, Next: http.NotFoundHandler()})
		if err != nil {
			t.Fatal(err)
		}
		for candidate := 0; ; candidate++ {
			name := fmt.Sprintf("team-%d", candidate)
			owner, err := router.Owner(name)
			if err != nil {
				t.Fatal(err)
			}
			if owner == shard.ShardID(i) {
				names[i] = name
				break
			}
		}
	}
	cookie, csrf := groupSession(t, services[0], "alice", "alice-id")
	for entryID := range services {
		entry, err := proxy.NewHandler(proxy.HandlerOptions{LocalShard: shard.ShardID(entryID), Router: services[entryID], Next: services[entryID],
			Resolver: resolverFunc(func(id shard.ShardID) (*url.URL, error) {
				return url.Parse(fmt.Sprintf("http://gitone-%d:8080", id))
			}),
			Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				var owner int
				if _, err := fmt.Sscanf(r.URL.Host, "gitone-%d:8080", &owner); err != nil || owner < 0 || owner >= len(services) {
					t.Fatalf("invalid forwarding host: %s", r.URL.Host)
				}
				w := httptest.NewRecorder()
				services[owner].ServeHTTP(w, r)
				return w.Result(), nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		for owner, name := range names {
			expect := http.StatusConflict
			if entryID == 0 {
				expect = http.StatusCreated
			}
			w := groupRequest(entry, "POST", "/api/v1/groups/"+name, "", cookie, csrf)
			if w.Code != expect {
				t.Fatalf("entry %d owner %d creation=%d: %s", entryID, owner, w.Code, w.Body.String())
			}
			w = groupRequest(entry, "GET", fmt.Sprintf("/api/v1/spaces?shard=%d", owner), "", cookie, csrf)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"`+name+`"`) {
				t.Fatalf("entry %d owner %d discovery=%d: %s", entryID, owner, w.Code, w.Body.String())
			}
			w = groupRequest(entry, "GET", "/api/v1/groups/"+name, "", cookie, csrf)
			if w.Code != 200 {
				t.Fatalf("entry %d owner %d read=%d: %s", entryID, owner, w.Code, w.Body.String())
			}
		}
	}
}
