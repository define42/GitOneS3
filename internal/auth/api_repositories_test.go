package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestAPIRepositoryPersonalLifecycle(t *testing.T) {
	t.Parallel()
	objects := storage.NewMemoryStore()
	s := testService(t, 1, objects, &fakeProvider{}, nil)
	if err := s.bindUser(context.Background(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	check := repositoryRequestChecker(t, s, cookie, csrf)
	listed := check("GET", "/api/v1/repos/alice", "", 200)
	if !strings.Contains(listed.Body.String(), `"repositories":[]`) {
		t.Fatalf("empty list: %s", listed.Body.String())
	}
	created := check("POST", "/api/v1/repos/alice", `{"name":"hello-world","description":"A private project","initializeReadme":true}`, 201)
	var view repositoryView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID == "" || view.Namespace != "alice" || view.Name != "hello-world" || view.IsEmpty ||
		view.DefaultBranch != "main" || view.Visibility != "private" || !view.CanWrite || view.Role != "owner" ||
		created.Header().Get("Location") != "/alice/hello-world" {
		t.Fatalf("creation: %+v headers=%v", view, created.Header())
	}
	check("POST", "/api/v1/repos/alice", `{"name":"hello-world"}`, 409)
	check("POST", "/api/v1/repos/alice", `{"name":"empty"}`, 201)
	listed = check("GET", "/api/v1/repos/alice", "", 200)
	if !strings.Contains(listed.Body.String(), `"name":"hello-world"`) || !strings.Contains(listed.Body.String(), `"name":"empty"`) {
		t.Fatalf("list missing repositories: %s", listed.Body.String())
	}
	base := "/api/v1/repos/alice/hello-world"
	check("GET", base, "", 200)
	branches := check("GET", base+"/branches", "", 200)
	if !strings.Contains(branches.Body.String(), `"name":"main"`) {
		t.Fatalf("branches: %s", branches.Body.String())
	}
	tree := check("GET", base+"/tree?ref=main", "", 200)
	if !strings.Contains(tree.Body.String(), `"name":"README.md"`) || !strings.Contains(tree.Body.String(), `"type":"file"`) {
		t.Fatalf("tree: %s", tree.Body.String())
	}
	blobResponse := check("GET", base+"/blob?ref=main&path=README.md", "", 200)
	var blob repository.Blob
	if err := json.Unmarshal(blobResponse.Body.Bytes(), &blob); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob.Content, "# hello-world") || !strings.Contains(blob.Content, "A private project") || blob.IsBinary {
		t.Fatalf("readme: %+v", blob)
	}
	commits := check("GET", base+"/commits", "", 200)
	if !strings.Contains(commits.Body.String(), `"authorName":"alice"`) || !strings.Contains(commits.Body.String(), blob.Commit) {
		t.Fatalf("history: %s", commits.Body.String())
	}
	for _, suffix := range []string{"", "/branches", "/tree", "/commits"} {
		check("GET", "/api/v1/repos/alice/empty"+suffix, "", 200)
	}
	check("GET", "/api/v1/repos/alice/empty/blob?path=README.md", "", 404)
	check("GET", base+"/tree?ref=missing", "", 404)
	check("GET", base+"/blob?path=missing", "", 404)
	check("GET", "/api/v1/repos/alice/missing", "", 404)
	// Replacing a shard process does not lose its repository catalog or contents.
	restarted := testService(t, 1, objects, &fakeProvider{}, nil)
	repositoryRequestChecker(t, restarted, cookie, csrf)("GET", base+"/blob?path=README.md", "", 200)
	// A different username for the same identity does not unlock a personal space.
	alias, aliasCSRF := groupSession(t, s, "alias", "alice-id")
	repositoryRequestChecker(t, s, alias, aliasCSRF)("GET", base, "", 403)
}

func TestAPIRepositoryRolesAndRevocation(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	owner, ownerCSRF := groupSession(t, s, "alice", "alice-id")
	reader, readerCSRF := groupSession(t, s, "bob", "bob-id")
	outsider, outsiderCSRF := groupSession(t, s, "eve", "eve-id")
	ownerCheck := repositoryRequestChecker(t, s, owner, ownerCSRF)
	readerCheck := repositoryRequestChecker(t, s, reader, readerCSRF)
	outsiderCheck := repositoryRequestChecker(t, s, outsider, outsiderCSRF)
	ownerCheck("POST", "/api/v1/groups/acme", "", 201)
	ownerCheck("POST", "/api/v1/repos/acme", `{"name":"project","initializeReadme":true}`, 201)
	base := "/api/v1/repos/acme/project"
	paths := []string{"/api/v1/repos/acme", base, base + "/branches", base + "/tree", base + "/blob?path=README.md", base + "/commits"}
	ownerCheck("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:bob-id","role":"reader"}`, 200)
	for _, path := range paths {
		outsiderCheck("GET", path, "", 403)
		readerCheck("GET", path, "", 403)
		repositoryRequestChecker(t, s, nil, "")("GET", path, "", 401)
	}
	readerCheck("POST", "/api/v1/groups/acme/invitations/accept", "", 200)
	for _, path := range paths {
		readerCheck("GET", path, "", 200)
	}
	read := readerCheck("GET", base, "", 200)
	if !strings.Contains(read.Body.String(), `"canWrite":false`) || !strings.Contains(read.Body.String(), `"role":"reader"`) {
		t.Fatalf("reader permissions: %s", read.Body.String())
	}
	readerCheck("POST", "/api/v1/repos/acme", `{"name":"denied"}`, 403)
	ownerCheck("PUT", "/api/v1/groups/acme/members", `{"userId":"google:bob-id","role":"developer"}`, 200)
	readerCheck("POST", "/api/v1/repos/acme", `{"name":"developer-project"}`, 201)
	ownerCheck("PUT", "/api/v1/groups/acme/members", `{"userId":"google:bob-id","role":"reader"}`, 200)
	readerCheck("POST", "/api/v1/repos/acme", `{"name":"demoted"}`, 403)
	ownerCheck("DELETE", "/api/v1/groups/acme/members", `{"userId":"google:bob-id"}`, 200)
	for _, path := range paths {
		readerCheck("GET", path, "", 403)
	}
}

func TestAPIRepositoryValidationAndCSRF(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	cookie, csrf := groupSession(t, s, "alice", "alice-id")
	check := repositoryRequestChecker(t, s, cookie, csrf)
	check("POST", "/api/v1/groups/acme", "", 201)
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{"missing name", `{}`, 422},
		{"unknown field", `{"name":"project","visibility":"public"}`, 422},
		{"uppercase name", `{"name":"Project"}`, 422},
		{"traversal name", `{"name":"../project"}`, 422},
		{"git suffix", `{"name":"project.git"}`, 400},
		{"reserved name", `{"name":"settings"}`, 400},
		{"double dot", `{"name":"pro..ject"}`, 400},
		{"invalid branch", `{"name":"project","defaultBranch":"../evil"}`, 400},
		{"oversized body", `{"name":"project","description":"` + strings.Repeat("x", 5000) + `"}`, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			check("POST", "/api/v1/repos/acme", test.body, test.want)
		})
	}
	for _, test := range []struct{ name, origin, token string }{
		{"missing token", "https://git.example", ""},
		{"wrong token", "https://git.example", "wrong"},
		{"missing origin", "", csrf},
		{"wrong origin", "https://evil.example", csrf},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/repos/acme", strings.NewReader(`{"name":"project"}`))
			r.AddCookie(cookie)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-CSRF-Token", test.token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("unsafe create: %d %s", w.Code, w.Body.String())
			}
		})
	}
	check("POST", "/api/v1/repos/acme", `{"name":"project","initializeReadme":true}`, 201)
	for _, path := range []string{"../auth/users/alice.json", "/README.md", "a//b", "a/../README.md", "a%5Cb", "a%00b"} {
		check("GET", "/api/v1/repos/acme/project/blob?path="+path, "", 400)
	}
	check("GET", "/api/v1/repos/acme/project.git", "", 400)
}

func TestUIRepositoryReturnPaths(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, test := range []struct {
		path string
		want bool
	}{
		{"/auth/new-repository", true},
		{"/auth/new-repository?namespace=alice", true},
		{"/alice/project", true},
		{"/alice/project/", true},
		{"/acme/project?ref=main&path=README.md", true},
		{"/acme/project?ref=feature%2Fwork&path=docs%2Fread%20me.md", true},
		{"/acme/project?view=commits", true},
		{"//evil.example/project", false},
		{"/alice/project.git", false},
		{"/alice/project?next=https://evil.example", false},
		{"/alice/project?view=other", false},
		{"/alice/project?ref=main&ref=other", false},
		{"/alice/project?path=%0d%0aLocation:evil", false},
		{"/alice/project?path=a%5Cb", false},
		{"/alice/project#x", false},
		{"/alice/%70roject", false},
		{"/auth/new-repository?namespace=api", false},
		{"/auth/new-repository?namespace=auth", false},
		{"/auth/new-repository?namespace=alice&namespace=bob", false},
		{"/auth/new-repository?next=evil", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := s.validReturnTo(test.path); got != test.want {
				t.Fatalf("validReturnTo(%q)=%v, want %v", test.path, got, test.want)
			}
		})
	}
}

func repositoryRequestChecker(t *testing.T, handler http.Handler, cookie *http.Cookie, csrf string) func(string, string, string, int) *httptest.ResponseRecorder {
	t.Helper()
	return func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := groupRequest(handler, method, path, body, cookie, csrf)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if cache := w.Header().Get("Cache-Control"); cache != "no-store" {
			t.Fatalf("private repository response must not be cached: %q", cache)
		}
		return w
	}
}
