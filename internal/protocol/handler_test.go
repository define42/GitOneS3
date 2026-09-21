package protocol

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		target     string
		wantEngine string
		wantStatus int
	}{
		{
			name:       "upload pack advertisement",
			method:     http.MethodGet,
			target:     "/acme/repo.git/info/refs?service=git-upload-pack",
			wantEngine: "git",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "receive pack",
			method:     http.MethodPost,
			target:     "/acme/repo.git/git-receive-pack",
			wantEngine: "git",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "lfs batch",
			method:     http.MethodPost,
			target:     "/acme/repo.git/info/lfs/objects/batch",
			wantEngine: "lfs",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "unknown route",
			method:     http.MethodGet,
			target:     "/acme/repo.git/archive.zip",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid advertisement service",
			method:     http.MethodGet,
			target:     "/acme/repo.git/info/refs?service=invalid",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			git := engineHandler("git")
			lfs := engineHandler("lfs")
			handler := NewHandler(git, lfs)
			request := httptest.NewRequest(test.method, test.target, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if got := response.Code; got != test.wantStatus {
				t.Fatalf("status = %d, want %d", got, test.wantStatus)
			}
			if test.wantEngine != "" {
				if got := response.Header().Get("X-Test-Engine"); got != test.wantEngine {
					t.Fatalf("engine = %q, want %q", got, test.wantEngine)
				}
			}
		})
	}
}

func TestHandler_UnimplementedEngine(t *testing.T) {
	t.Parallel()

	handler := NewHandler(nil, nil)
	request := httptest.NewRequest(
		http.MethodPost,
		"/acme/repo.git/git-upload-pack",
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if got, want := response.Code, http.StatusNotImplemented; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func engineHandler(name string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("X-Test-Engine", name)
		response.WriteHeader(http.StatusNoContent)
	})
}
