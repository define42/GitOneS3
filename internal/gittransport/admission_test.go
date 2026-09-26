package gittransport

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestAdmissionOptions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options []Options
		want    Options
		invalid bool
	}{
		{name: "default", want: Options{1, 4, 5 * time.Second}},
		{name: "explicit fail fast", options: []Options{{}}, want: Options{1, 0, 5 * time.Second}},
		{name: "configured", options: []Options{{2, 3, time.Second}}, want: Options{2, 3, time.Second}},
		{name: "negative active", options: []Options{{MaxConcurrentOperations: -1}}, invalid: true},
		{name: "too many active", options: []Options{{MaxConcurrentOperations: 33}}, invalid: true},
		{name: "negative queue", options: []Options{{MaxQueuedOperations: -1}}, invalid: true},
		{name: "too many queued", options: []Options{{MaxQueuedOperations: 1025}}, invalid: true},
		{name: "negative timeout", options: []Options{{QueueTimeout: -time.Second}}, invalid: true},
		{name: "too long timeout", options: []Options{{QueueTimeout: 91 * time.Second}}, invalid: true},
		{name: "ambiguous options", options: []Options{{}, {}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := admissionOptions(test.options)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid limits accepted")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("options = %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}

func TestAdmissionBoundsActiveWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		limit int
	}{{"one", 1}, {"two", 2}, {"four", 4}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gate := newAdmission(test.limit, 0, time.Second)
			for range test.limit {
				release, err := gate.acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if release, err := gate.acquire(t.Context()); !errors.Is(err, errBusy) || release != nil {
				t.Fatalf("excess operation admitted: %v", err)
			}
		})
	}
}

func TestAdmissionBoundedQueueAndHandoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		gate := newAdmission(1, 1, 5*time.Second)
		release, err := gate.acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			release, err := gate.acquire(t.Context())
			if err == nil {
				release()
			}
			finished <- err
		}()
		synctest.Wait()
		if otherRelease, err := gate.acquire(t.Context()); !errors.Is(err, errBusy) || otherRelease != nil {
			t.Fatalf("full queue did not reject request: %v", err)
		}
		release()
		if err := <-finished; err != nil {
			t.Fatalf("queued operation did not take released slot: %v", err)
		}
		if len(gate.active) != 0 || len(gate.waiting) != 0 {
			t.Fatal("admission capacity leaked")
		}
	})
}

func TestAdmissionQueueCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cancel", "caller deadline", "queue timeout"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				gate := newAdmission(1, 1, 5*time.Second)
				release, err := gate.acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				if name == "caller deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				}
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					release, err := gate.acquire(ctx)
					if release != nil {
						release()
					}
					finished <- err
				}()
				synctest.Wait()
				want := errBusy
				switch name {
				case "cancel":
					cancel()
					want = context.Canceled
				case "caller deadline":
					time.Sleep(time.Second)
					want = context.DeadlineExceeded
				case "queue timeout":
					time.Sleep(5 * time.Second)
				}
				if err := <-finished; !errors.Is(err, want) {
					t.Fatalf("waiting request error = %v, want %v", err, want)
				}
				if len(gate.waiting) != 0 || len(gate.active) != 1 {
					t.Fatal("canceled waiter leaked capacity or consumed an active slot")
				}
			})
		})
	}
}

func TestAdmissionCanceledCallerDoesNoWork(t *testing.T) {
	t.Parallel()
	gate := newAdmission(1, 1, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if release, err := gate.acquire(ctx); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("canceled caller admitted: %v", err)
	}
	if len(gate.active) != 0 || len(gate.waiting) != 0 {
		t.Fatal("canceled caller consumed capacity")
	}
}
