// Package protocol dispatches owner-shard Git Smart HTTP and Git LFS routes.
package protocol

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// Handler dispatches standard client routes to replaceable Git and LFS
// engines. Namespace routing and authorization run before this handler.
type Handler struct {
	git http.Handler
	lfs http.Handler
}

// NewHandler constructs an owner-shard protocol dispatcher. Nil engines are
// represented by an explicit 501 response so local disk is never used as an
// accidental authoritative fallback.
func NewHandler(git, lfs http.Handler) *Handler {
	if git == nil {
		git = Unimplemented("git")
	}
	if lfs == nil {
		lfs = Unimplemented("lfs")
	}

	return &Handler{git: git, lfs: lfs}
}

// ServeHTTP recognizes the protocol endpoints required by ordinary clients.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if isLFSPath(path) {
		h.lfs.ServeHTTP(response, request)
		return
	}
	if isGitRequest(request) {
		h.git.ServeHTTP(response, request)
		return
	}

	http.NotFound(response, request)
}

// Unimplemented returns a stable owner-side response for a protocol engine
// that has not yet been installed.
func Unimplemented(engine string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotImplemented)
		if err := json.NewEncoder(response).Encode(map[string]string{
			"error":  "not_implemented",
			"engine": engine,
		}); err != nil {
			return
		}
	})
}

func isGitRequest(request *http.Request) bool {
	path := request.URL.Path
	switch request.Method {
	case http.MethodGet:
		if !strings.HasSuffix(path, ".git/info/refs") {
			return false
		}
		service := request.URL.Query().Get("service")
		return service == uploadPack || service == receivePack
	case http.MethodPost:
		return strings.HasSuffix(path, ".git/"+uploadPack) ||
			strings.HasSuffix(path, ".git/"+receivePack)
	default:
		return false
	}
}

func isLFSPath(path string) bool {
	marker := ".git/info/lfs/"
	return strings.Contains(path, marker) && !strings.HasSuffix(path, marker)
}
