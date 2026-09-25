package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/define42/GitOneS3/internal/shard"
)

func TestNewHandler(t *testing.T) {
	t.Parallel()
	parser := testParser(t)
	next := http.NotFoundHandler()
	if _, err := NewHandler(next, parser); err != nil {
		t.Fatalf("create embedded handler: %v", err)
	}
	if _, err := newHandler(nil, parser, fstest.MapFS{}); err == nil {
		t.Error("nil next handler accepted")
	}
	if _, err := newHandler(next, nil, fstest.MapFS{}); err == nil {
		t.Error("nil namespace parser accepted")
	}
}

func TestNewHandlerMissingBuild(t *testing.T) {
	t.Parallel()
	h, err := newHandler(http.NotFoundHandler(), testParser(t), fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "make ui") {
		t.Fatalf("missing build response = %d %s", response.Code, response.Body.String())
	}
}

func TestHandlerServeHTTP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		accept string
		shell  bool
	}{
		{name: "root", method: http.MethodGet, path: "/", shell: true},
		{name: "login", method: http.MethodGet, path: "/auth/login", accept: "text/html", shell: true},
		{name: "register", method: http.MethodGet, path: "/auth/register", accept: "text/html", shell: true},
		{name: "new group", method: http.MethodGet, path: "/auth/new-group", accept: "text/html", shell: true},
		{name: "token management", method: http.MethodGet, path: "/auth/tokens", accept: "text/html", shell: true},
		{name: "SSH key management", method: http.MethodGet, path: "/auth/ssh-keys", accept: "text/html", shell: true},
		{name: "SSH key management json", method: http.MethodGet, path: "/auth/ssh-keys", accept: "application/json"},
		{name: "token management head", method: http.MethodHead, path: "/auth/tokens", accept: "text/html", shell: true},
		{name: "token management json", method: http.MethodGet, path: "/auth/tokens", accept: "application/json"},
		{name: "token management post", method: http.MethodPost, path: "/auth/tokens", accept: "text/html"},
		{name: "personal space", method: http.MethodGet, path: "/alice", accept: "text/html", shell: true},
		{name: "space slash", method: http.MethodGet, path: "/alice/", accept: "text/html", shell: true},
		{name: "head", method: http.MethodHead, path: "/alice", accept: "text/html", shell: true},
		{name: "group settings", method: http.MethodGet, path: "/team/settings", accept: "text/html", shell: true},
		{name: "invitation", method: http.MethodGet, path: "/team/invitations/accept", accept: "text/html", shell: true},
		{name: "browser accept", method: http.MethodGet, path: "/alice", accept: "text/html,application/xhtml+xml;q=0.9", shell: true},
		{name: "root post", method: http.MethodPost, path: "/", accept: "text/html"},
		{name: "group create", method: http.MethodPost, path: "/team", accept: "text/html"},
		{name: "legacy json", method: http.MethodGet, path: "/alice", accept: "application/json"},
		{name: "no html preference", method: http.MethodGet, path: "/alice", accept: "*/*"},
		{name: "html disabled", method: http.MethodGet, path: "/alice", accept: "text/html;q=0.000"},
		{name: "html invalid quality", method: http.MethodGet, path: "/alice", accept: "text/html;q=garbage"},
		{name: "api", method: http.MethodGet, path: "/api/v1/session", accept: "text/html"},
		{name: "api docs", method: http.MethodGet, path: "/api/docs", accept: "text/html"},
		{name: "oidc callback", method: http.MethodGet, path: "/auth/oidc/callback?code=secret", accept: "text/html"},
		{name: "google callback", method: http.MethodGet, path: "/auth/google/callback", accept: "text/html"},
		{name: "oidc start", method: http.MethodGet, path: "/alice/auth/oidc/login", accept: "text/html"},
		{name: "session", method: http.MethodGet, path: "/alice/auth/session", accept: "text/html"},
		{name: "git", method: http.MethodGet, path: "/alice/repo.git/info/refs", accept: "text/html"},
		{name: "lfs", method: http.MethodGet, path: "/alice/repo.git/info/lfs/objects/batch", accept: "text/html"},
		{name: "unknown page", method: http.MethodGet, path: "/alice/unknown/path", accept: "text/html"},
		{name: "reserved api", method: http.MethodGet, path: "/api", accept: "text/html"},
		{name: "reserved gitone", method: http.MethodGet, path: "/gitone", accept: "text/html"},
		{name: "reserved system", method: http.MethodGet, path: "/system", accept: "text/html"},
		{name: "reserved auth", method: http.MethodGet, path: "/auth", accept: "text/html"},
		{name: "encoded username", method: http.MethodGet, path: "/%61lice", accept: "text/html"},
		{name: "encoded slash", method: http.MethodGet, path: "/alice%2fsettings", accept: "text/html"},
		{name: "traversal", method: http.MethodGet, path: "/alice/../auth/login", accept: "text/html"},
		{name: "double slash", method: http.MethodGet, path: "//alice", accept: "text/html"},
		{name: "case ambiguity", method: http.MethodGet, path: "/Alice", accept: "text/html"},
		{name: "invalid namespace", method: http.MethodGet, path: "/-alice", accept: "text/html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := testHandler(t, testFiles())
			request := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			request.Header.Set("Accept", tt.accept)
			request.Header.Set("Cookie", "session=opaque")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if !tt.shell {
				if response.Code != http.StatusTeapot || response.Header().Get("X-Next-Cookie") != "session=opaque" {
					t.Fatalf("request was intercepted: %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("HTML response must not be cached")
			}
			if response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Error("HTML content type missing")
			}
			if response.Header().Get("Vary") != "Accept" {
				t.Error("content negotiation not reflected in Vary")
			}
			if tt.method == http.MethodHead && response.Body.Len() != 0 {
				t.Error("HEAD returned a body")
			}
			if tt.method != http.MethodHead && response.Body.String() != "<!doctype html><title>GitOne</title>" {
				t.Error("wrong shell body")
			}
			assertSecurityHeaders(t, response)
		})
	}
}

func TestHandlerRepositoryNavigation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		accept string
		shell  bool
	}{
		{
			name: "create repository", method: http.MethodGet,
			path: "/auth/new-repository", accept: "text/html", shell: true,
		},
		{
			name: "create in namespace", method: http.MethodGet,
			path: "/auth/new-repository?namespace=alice", accept: "text/html", shell: true,
		},
		{
			name: "personal repository", method: http.MethodGet,
			path: "/alice/my-project", accept: "text/html", shell: true,
		},
		{
			name: "group repository slash", method: http.MethodGet,
			path: "/team/my-project/", accept: "text/html", shell: true,
		},
		{
			name: "repository head", method: http.MethodHead,
			path: "/alice/my-project", accept: "text/html", shell: true,
		},
		{
			name: "browse branch and file", method: http.MethodGet,
			path: "/alice/my-project?ref=feature%2Fdocs&path=docs%2FREADME.md", accept: "text/html", shell: true,
		},
		{
			name: "commit history", method: http.MethodGet,
			path: "/team/my-project?ref=main&view=commits", accept: "text/html", shell: true,
		},
		{
			name: "repository punctuation", method: http.MethodGet,
			path: "/alice/my_project.v1", accept: "text/html", shell: true,
		},
		{
			name: "single character", method: http.MethodGet,
			path: "/alice/a", accept: "text/html", shell: true,
		},
		{
			name: "maximum length", method: http.MethodGet,
			path: "/alice/" + strings.Repeat("a", 63), accept: "text/html", shell: true,
		},
		{
			name: "existing settings page", method: http.MethodGet,
			path: "/team/settings", accept: "text/html", shell: true,
		},
		{
			name: "existing invitation page", method: http.MethodGet,
			path: "/team/invitations/accept", accept: "text/html", shell: true,
		},
		{
			name: "json repository request", method: http.MethodGet,
			path: "/alice/my-project", accept: "application/json",
		},
		{
			name: "unspecified accept", method: http.MethodGet,
			path: "/alice/my-project",
		},
		{
			name: "wildcard accept", method: http.MethodGet,
			path: "/alice/my-project", accept: "*/*",
		},
		{
			name: "html disabled", method: http.MethodGet,
			path: "/alice/my-project", accept: "text/html;q=0",
		},
		{
			name: "post repository", method: http.MethodPost,
			path: "/alice/my-project", accept: "text/html",
		},
		{
			name: "create requires html", method: http.MethodGet,
			path: "/auth/new-repository?namespace=alice", accept: "application/json",
		},
		{
			name: "create rejects post", method: http.MethodPost,
			path: "/auth/new-repository", accept: "text/html",
		},
		{
			name: "git suffix", method: http.MethodGet,
			path: "/alice/my-project.git", accept: "text/html",
		},
		{
			name: "git suffix slash", method: http.MethodGet,
			path: "/alice/my-project.git/", accept: "text/html",
		},
		{
			name: "git discovery", method: http.MethodGet,
			path: "/alice/my-project.git/info/refs?service=git-upload-pack", accept: "text/html",
		},
		{
			name: "git rpc", method: http.MethodPost,
			path: "/alice/my-project.git/git-receive-pack", accept: "text/html",
		},
		{
			name: "suffixless git discovery", method: http.MethodGet,
			path: "/alice/my-project/info/refs", accept: "text/html",
		},
		{
			name: "lfs request", method: http.MethodPost,
			path: "/alice/my-project.git/info/lfs/objects/batch", accept: "text/html",
		},
		{
			name: "repository api", method: http.MethodGet,
			path: "/api/v1/repos/alice/my-project", accept: "text/html",
		},
		{
			name: "reserved auth", method: http.MethodGet,
			path: "/alice/auth", accept: "text/html",
		},
		{
			name: "reserved members", method: http.MethodGet,
			path: "/team/members", accept: "text/html",
		},
		{
			name: "reserved invitations", method: http.MethodGet,
			path: "/team/invitations", accept: "text/html",
		},
		{
			name: "leading punctuation", method: http.MethodGet,
			path: "/alice/.hidden", accept: "text/html",
		},
		{
			name: "trailing punctuation", method: http.MethodGet,
			path: "/alice/project_", accept: "text/html",
		},
		{
			name: "repository case", method: http.MethodGet,
			path: "/alice/My-project", accept: "text/html",
		},
		{
			name: "consecutive dots", method: http.MethodGet,
			path: "/alice/my..project", accept: "text/html",
		},
		{
			name: "too long", method: http.MethodGet,
			path: "/alice/" + strings.Repeat("a", 64), accept: "text/html",
		},
		{
			name: "encoded repository", method: http.MethodGet,
			path: "/alice/%6dy-project", accept: "text/html",
		},
		{
			name: "encoded dot", method: http.MethodGet,
			path: "/alice/my-project%2egit", accept: "text/html",
		},
		{
			name: "encoded separator", method: http.MethodGet,
			path: "/alice/my-project%2fmain", accept: "text/html",
		},
		{
			name: "double slash", method: http.MethodGet,
			path: "/alice//my-project", accept: "text/html",
		},
		{
			name: "traversal", method: http.MethodGet,
			path: "/alice/../my-project", accept: "text/html",
		},
		{
			name: "invalid namespace", method: http.MethodGet,
			path: "/Alice/my-project", accept: "text/html",
		},
		{
			name: "reserved namespace", method: http.MethodGet,
			path: "/system/my-project", accept: "text/html",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			request.Header.Set("Accept", tt.accept)
			request.Header.Set("Cookie", "session=opaque")
			response := httptest.NewRecorder()
			testHandler(t, testFiles()).ServeHTTP(response, request)
			if !tt.shell {
				if response.Code != http.StatusTeapot || response.Header().Get("X-Next-Cookie") != "session=opaque" {
					t.Fatalf("request was intercepted: %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("expected HTML shell, got %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("repository HTML must not be cached")
			}
			if tt.method == http.MethodHead && response.Body.Len() != 0 {
				t.Error("HEAD returned a body")
			}
			assertSecurityHeaders(t, response)
		})
	}
}

func TestHandlerServeAsset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "javascript", method: http.MethodGet, path: "/gitone/assets/app-abcd.js", status: http.StatusOK},
		{name: "head", method: http.MethodHead, path: "/gitone/assets/app-abcd.js", status: http.StatusOK},
		{name: "post", method: http.MethodPost, path: "/gitone/assets/app-abcd.js", status: http.StatusMethodNotAllowed},
		{name: "missing", method: http.MethodGet, path: "/gitone/assets/missing.js", status: http.StatusNotFound},
		{name: "directory", method: http.MethodGet, path: "/gitone/assets/", status: http.StatusNotFound},
		{name: "traversal", method: http.MethodGet, path: "/gitone/assets/../index.html", status: http.StatusNotFound},
		{name: "encoded traversal", method: http.MethodGet, path: "/gitone/assets/%2e%2e/index.html", status: http.StatusNotFound},
		{name: "encoded slash", method: http.MethodGet, path: "/gitone/assets%2fapp-abcd.js", status: http.StatusNotFound},
		{name: "backslash", method: http.MethodGet, path: "/gitone/assets/..%5cindex.html", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := testHandler(t, testFiles())
			request := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			request.Header.Set("Accept", "text/html")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.status, response.Body.String())
			}
			if tt.method == http.MethodHead && response.Body.Len() != 0 {
				t.Error("HEAD returned a body")
			}
			if tt.status == http.StatusOK {
				if response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
					t.Error("hashed asset is not immutable")
				}
				if !strings.Contains(response.Header().Get("Content-Type"), "javascript") {
					t.Error("JavaScript content type missing")
				}
			} else if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("asset errors must not be cached")
			}
			assertSecurityHeaders(t, response)
		})
	}
}

func testHandler(t *testing.T, files fs.FS) *handler {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Cookie", r.Header.Get("Cookie"))
		w.WriteHeader(http.StatusTeapot)
	})
	h, err := newHandler(next, testParser(t), files)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func testParser(t *testing.T) *shard.Parser {
	t.Helper()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return parser
}

func testFiles() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>GitOne</title>")},
		"assets/app-abcd.js": {Data: []byte("console.log('GitOne');")},
	}
}

func assertSecurityHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
	if response.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("frame protection missing")
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("referrer protection missing")
	}
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "script-src 'self'") || strings.Contains(policy, "unsafe-inline") {
		t.Errorf("unexpected CSP: %q", policy)
	}
}
