// Package webui serves the bundled browser interface without intercepting API,
// OIDC, Git, or LFS requests.
package webui

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
)

// A tracked README keeps ordinary Go tests independent of Node. Release builds
// populate this directory with the TypeScript application's compiled output.
//
//go:embed dist
var bundled embed.FS

const missingBuild = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>GitOne UI build required</title></head>
<body><h1>GitOne UI build required</h1><p>Run make ui before starting the Go process, or use make run.</p></body></html>`

type handler struct {
	next   http.Handler
	parser *shard.Parser
	files  fs.FS
	index  []byte
	status int
}

// NewHandler adds browser navigation and bundled assets to an existing handler.
// Private data remains behind the authenticated API; the HTML shell is public.
func NewHandler(next http.Handler, parser *shard.Parser) (http.Handler, error) {
	files, err := fs.Sub(bundled, "dist")
	if err != nil {
		return nil, fmt.Errorf("open bundled UI: %w", err)
	}
	return newHandler(next, parser, files)
}

func newHandler(next http.Handler, parser *shard.Parser, files fs.FS) (*handler, error) {
	if next == nil || parser == nil {
		return nil, errors.New("UI requires a next handler and namespace parser")
	}
	index, err := fs.ReadFile(files, "index.html")
	status := http.StatusOK
	if errors.Is(err, fs.ErrNotExist) {
		index = []byte(missingBuild)
		status = http.StatusServiceUnavailable
	} else if err != nil {
		return nil, fmt.Errorf("read bundled UI index: %w", err)
	}
	return &handler{next: next, parser: parser, files: files, index: index, status: status}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/gitone/assets/") {
		h.serveAsset(w, r)
		return
	}
	if !h.isNavigation(r) {
		h.next.ServeHTTP(w, r)
		return
	}
	securityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept")
	w.WriteHeader(h.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(h.index)
	}
}

func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/gitone/")
	if !canonicalPath(r) || !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(h.files, name)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	content, err := fs.ReadFile(h.files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Vite emits content-hashed asset names. The HTML shell is never cached.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

func (h *handler) isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !canonicalPath(r) {
		return false
	}
	if r.URL.Path == "/" {
		return true
	}
	if !acceptsHTML(r.Header.Get("Accept")) {
		return false
	}
	switch r.URL.Path {
	case "/auth/login", "/auth/register", "/auth/new-group", "/auth/new-repository", "/auth/tokens":
		return true
	}

	trimmed := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(trimmed, "/"), "/")
	if parts[0] == "auth" {
		return false
	}
	groupSettings := len(parts) == 2 && parts[1] == "settings"
	invitation := len(parts) == 3 && parts[1] == "invitations" && parts[2] == "accept"
	repositoryPage := len(parts) == 2 && repository.ValidName(parts[1])
	if len(parts) != 1 && !groupSettings && !invitation && !repositoryPage {
		return false
	}
	// Validate the same namespace syntax and reserved names as shard routing.
	namespaceRequest := r.Clone(r.Context())
	namespaceRequest.URL.Path = "/" + parts[0]
	namespaceRequest.URL.RawPath = ""
	namespaceRequest.RequestURI = namespaceRequest.URL.Path
	_, err := h.parser.Parse(namespaceRequest)
	return err == nil
}

func canonicalPath(r *http.Request) bool {
	if r.URL.Opaque != "" || r.URL.EscapedPath() != r.URL.Path {
		return false
	}
	if strings.ContainsAny(r.URL.Path, "\\\x00") {
		return false
	}
	requestPath, _, _ := strings.Cut(r.RequestURI, "?")
	return requestPath == "" || requestPath == r.URL.Path
}

func acceptsHTML(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || mediaType != "text/html" {
			continue
		}
		if quality, exists := params["q"]; exists {
			value, err := strconv.ParseFloat(quality, 64)
			if err != nil || value <= 0 || value > 1 {
				continue
			}
		}
		return true
	}
	return false
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; "+
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; "+
		"form-action 'self'; frame-ancestors 'none'; object-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}
