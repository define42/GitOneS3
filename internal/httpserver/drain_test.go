package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestDrainRejectsLateWork(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	drain := newRequestDrain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	drain.stop()
	drain.stop()
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			defer func() {
				if got, ok := recover().(error); !ok || !errors.Is(got, http.ErrAbortHandler) {
					t.Errorf("late request panic = %v, want ErrAbortHandler", got)
				}
			}()
			drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		})
	}
	workers.Wait()
	if got := called.Load(); got != 0 {
		t.Fatalf("%d requests entered application code after drain began", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := drain.wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRequestDrainJoinsPanicCleanup(t *testing.T) {
	t.Parallel()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	drain := newRequestDrain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		defer func() { <-release; close(finished) }()
		close(entered)
		panic(http.ErrAbortHandler)
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if got, ok := recover().(error); !ok || !errors.Is(got, http.ErrAbortHandler) {
				t.Errorf("handler panic = %v", got)
			}
		}()
		drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	}()
	<-entered
	drain.stop()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := drain.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("unfinished panic cleanup was not tracked: %v", err)
	}
	close(release)
	<-done
	<-finished
	if err := drain.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRequestDrainConcurrentAdmissionAndStop(t *testing.T) {
	t.Parallel()
	start, release := make(chan struct{}), make(chan struct{})
	drain := newRequestDrain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			defer func() {
				if got := recover(); got != nil {
					if err, ok := got.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
						t.Errorf("unexpected request panic: %v", got)
					}
				}
			}()
			<-start
			drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		})
	}
	workers.Go(func() { <-start; drain.stop(); close(release) })
	close(start)
	workers.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Both channels are ready: completed cleanup must win over the deadline.
	if err := drain.wait(ctx); err != nil {
		t.Fatal(err)
	}
}
