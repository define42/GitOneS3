package gittransport

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Options bounds total Git work across HTTP and SSH on one shard. Raising the
// active limit requires measuring peak memory with the deployment's workloads;
// repository byte limits are not limits on the process's memory consumption.
type Options struct {
	MaxConcurrentOperations int
	MaxQueuedOperations     int
	QueueTimeout            time.Duration
}

func admissionOptions(options []Options) (Options, error) {
	if len(options) > 1 {
		return Options{}, errors.New("gittransport: at most one Options value is allowed")
	}
	result := Options{MaxConcurrentOperations: 1, MaxQueuedOperations: 4, QueueTimeout: 5 * time.Second}
	if len(options) == 1 {
		result = options[0]
		if result.MaxConcurrentOperations == 0 {
			result.MaxConcurrentOperations = 1
		}
		if result.QueueTimeout == 0 {
			result.QueueTimeout = 5 * time.Second
		}
	}
	if result.MaxConcurrentOperations < 1 || result.MaxConcurrentOperations > 32 ||
		result.MaxQueuedOperations < 0 || result.MaxQueuedOperations > 1024 ||
		result.QueueTimeout <= 0 || result.QueueTimeout > 90*time.Second {
		return Options{}, errors.New("gittransport: invalid operation admission limits")
	}
	return result, nil
}

var errBusy = errors.New("gittransport: git service is busy; retry shortly")

// admission bounds both active work and waiting requests. Callers acquire before
// loading object data or buffering a pack. It starts no background goroutines.
type admission struct {
	active  chan struct{}
	waiting chan struct{}
	timeout time.Duration
}

func newAdmission(active, queued int, timeout time.Duration) *admission {
	return &admission{
		active: make(chan struct{}, active), waiting: make(chan struct{}, queued), timeout: timeout,
	}
}

// acquire returns a release function that must be called exactly once on success.
func (a *admission) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release := func() { <-a.active }
	select {
	case a.active <- struct{}{}:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	default:
	}
	select {
	case a.waiting <- struct{}{}:
		defer func() { <-a.waiting }()
	default:
		return nil, errBusy
	}
	waitCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case a.active <- struct{}{}:
		// Cancellation and an available slot can become ready together. Do not
		// start expensive work after either the caller or queue deadline expired.
		if err := waitCtx.Err(); err == nil {
			return release, nil
		}
		release()
	case <-waitCtx.Done():
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: admission wait timed out", errBusy)
}
