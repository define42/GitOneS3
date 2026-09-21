package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestUserBindingIsPermanentAndAtomic(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, id := range []string{"google-a", "google-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.bindUser(context.Background(), "alice", Identity{Subject: id, Email: "same@example.com"})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, errUsernameTaken) {
				t.Errorf("bind: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful claims = %d", successes.Load())
	}
	data, _, err := s.readObject(context.Background(), "auth/users/alice.json")
	if err != nil || len(data) == 0 {
		t.Fatalf("missing durable binding: %v", err)
	}
}

func TestUserBindingUsesSubjectNotEmail(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	ctx := context.Background()
	if err := s.bindUser(ctx, "alice", Identity{Subject: "original", Email: "first@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.bindUser(ctx, "alice", Identity{Subject: "original", Email: "changed@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.bindUser(ctx, "alice", Identity{Subject: "attacker", Email: "first@example.com"}); !errors.Is(err, errUsernameTaken) {
		t.Fatalf("different subject claimed name: %v", err)
	}
}
