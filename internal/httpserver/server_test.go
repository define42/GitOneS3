package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		address   string
		handler   http.Handler
		wantError bool
	}{
		{name: "single listener", address: "127.0.0.1:0", handler: http.NotFoundHandler()},
		{name: "missing address", handler: http.NotFoundHandler(), wantError: true},
		{name: "missing handler", address: "127.0.0.1:0", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(test.address, test.handler, nil)
			if (err != nil) != test.wantError {
				t.Fatalf("New() error = %v, want error = %t", err, test.wantError)
			}
		})
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	t.Parallel()
	server, err := New("127.0.0.1:0", http.NotFoundHandler(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestRunReportsListenFailure(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server, err := New(listener.Addr().String(), http.NotFoundHandler(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded with an occupied port")
	}
}

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
