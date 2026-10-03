package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// requestDrain fences new application work and joins handlers, including their
// deferred cleanup. The mutex makes admission atomic with the start of drain.
// Tracking requests also covers protocols that multiplex them on a connection.
type requestDrain struct {
	next     http.Handler
	mu       sync.Mutex
	stopping bool
	active   int
	done     chan struct{}
}

func newRequestDrain(next http.Handler) *requestDrain {
	return &requestDrain{next: next, done: make(chan struct{})}
}

func (d *requestDrain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		// Abort without writing a response: net/http may otherwise drain a
		// stalled request body before writing. No application work was admitted.
		panic(http.ErrAbortHandler)
	}
	d.active++
	d.mu.Unlock()
	defer d.finish()
	d.next.ServeHTTP(w, r)
}

func (d *requestDrain) finish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	if d.stopping && d.active == 0 {
		close(d.done)
	}
}

func (d *requestDrain) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopping {
		d.stopping = true
		if d.active == 0 {
			close(d.done)
		}
	}
}

func (d *requestDrain) wait(ctx context.Context) error {
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		active := d.active
		finished := d.stopping && active == 0
		d.mu.Unlock()
		if finished {
			return nil
		}
		return fmt.Errorf("wait for %d HTTP request handlers to finish cleanup: %w", active, ctx.Err())
	}
}
