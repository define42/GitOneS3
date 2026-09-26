package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type lfsDeadlineWriter struct {
	*readDeadlineWriter
	writeDeadline time.Time
	writeErr      error
}

func (w *lfsDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.writeDeadline = deadline
	return nil
}

func TestHandlerLFSForwardingDeadline(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			response := &readDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || deadline.Before(before.Add(30*time.Minute)) || deadline.After(time.Now().Add(30*time.Minute)) {
					t.Errorf("LFS forwarding context deadline: %v", deadline)
				}
				if !response.deadline.Equal(deadline) {
					t.Errorf("body deadline %v differs from context %v", response.deadline, deadline)
				}
				return proxyResponse(http.StatusOK, ""), nil
			})
			h := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
			r := httptest.NewRequestWithContext(t.Context(), method, "/alice/repo.git/info/lfs/objects/"+strings.Repeat("a", 64), nil)
			h.ServeHTTP(response, r)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d", response.Code)
			}
		})
	}
}

func TestHandlerLFSDeadlineRespectsCallerAndCancelsAfterForwarding(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	response := &lfsDeadlineWriter{readDeadlineWriter: &readDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
	var forwarded context.Context
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		forwarded = r.Context()
		got, ok := forwarded.Deadline()
		if !ok || !got.Equal(deadline) || !response.deadline.Equal(deadline) || !response.writeDeadline.Equal(deadline) {
			t.Errorf("forwarding exceeded caller deadline: context=%v read=%v write=%v", got, response.deadline, response.writeDeadline)
		}
		return proxyResponse(http.StatusOK, ""), nil
	})
	h := proxyTestHandler(t, 0, transport, http.NotFoundHandler())
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/alice/repo.git/info/lfs/objects/"+strings.Repeat("a", 64), nil)
	h.ServeHTTP(response, r)
	if response.Code != http.StatusOK || forwarded == nil || !errors.Is(forwarded.Err(), context.Canceled) {
		t.Fatalf("forwarding did not release its context: status=%d context=%v", response.Code, forwarded)
	}
	if !response.writeDeadline.IsZero() || !response.deadline.Equal(deadline) {
		t.Fatal("write deadline was not cleared or finite unread-body deadline was lost")
	}
}

func TestHandlerLFSDeadlineFailureDoesNotForward(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		readErr, writeErr error
	}{
		{name: "read deadline", readErr: errors.New("read deadline unavailable")},
		{name: "write deadline", writeErr: errors.New("write deadline unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := &lfsDeadlineWriter{readDeadlineWriter: &readDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: test.readErr}, writeErr: test.writeErr}
			forwarded := false
			h := proxyTestHandler(t, 0, roundTripperFunc(func(*http.Request) (*http.Response, error) {
				forwarded = true
				return proxyResponse(http.StatusOK, ""), nil
			}), http.NotFoundHandler())
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/alice/repo.git/info/lfs/objects/"+strings.Repeat("a", 64), nil)
			h.ServeHTTP(response, r)
			if response.Code != http.StatusServiceUnavailable || forwarded {
				t.Fatalf("failed deadline admitted forwarding: status=%d forwarded=%v", response.Code, forwarded)
			}
		})
	}
}

func TestHandlerLFSCancellationReachesOwnerRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, done := make(chan struct{}), make(chan struct{})
	h := proxyTestHandler(t, 0, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}), http.NotFoundHandler())
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/alice/repo.git/info/lfs/objects/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.ServeHTTP(response, r)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("owner request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled LFS request continued forwarding")
	}
	if response.Code != http.StatusBadGateway {
		t.Fatalf("canceled forwarding status=%d", response.Code)
	}
}
