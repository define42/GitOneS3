package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		checker    Checker
		wantStatus int
		wantNext   bool
	}{
		{
			name:       "liveness",
			path:       "/livez",
			checker:    checkerFunc(func(context.Context) error { return nil }),
			wantStatus: http.StatusOK,
		},
		{
			name:       "ready",
			path:       "/readyz",
			checker:    checkerFunc(func(context.Context) error { return nil }),
			wantStatus: http.StatusOK,
		},
		{
			name:       "backing service unavailable",
			path:       "/readyz",
			checker:    checkerFunc(func(context.Context) error { return errors.New("unavailable") }),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "namespace request",
			path:       "/acme/repo.git/info/refs",
			checker:    checkerFunc(func(context.Context) error { return nil }),
			wantStatus: http.StatusAccepted,
			wantNext:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			called := false
			next := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				called = true
				response.WriteHeader(http.StatusAccepted)
			})
			handler := WithHealth(next, test.checker)
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if got := response.Code; got != test.wantStatus {
				t.Fatalf("status = %d, want %d", got, test.wantStatus)
			}
			if called != test.wantNext {
				t.Fatalf("next called = %t, want %t", called, test.wantNext)
			}
		})
	}
}

func TestStatusWriter_InformationalResponseDoesNotHideFinalStatus(t *testing.T) {
	t.Parallel()

	writer := &statusWriter{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusOK,
	}
	writer.WriteHeader(http.StatusEarlyHints)
	writer.WriteHeader(http.StatusUnauthorized)

	if got, want := writer.status, http.StatusUnauthorized; got != want {
		t.Fatalf("recorded status = %d, want %d", got, want)
	}
	if !writer.wroteHeader {
		t.Fatal("final response did not mark the header as written")
	}
}

type checkerFunc func(context.Context) error

func (f checkerFunc) Check(ctx context.Context) error {
	return f(ctx)
}
