package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestTokenLifecycleAndStoredVerifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	s := testService(t, 1, store, &fakeProvider{}, nil)
	identity := Identity{Subject: "alice-id", Email: "alice@example.com"}
	if err := s.bindUser(ctx, "alice", identity); err != nil {
		t.Fatal(err)
	}
	raw, view, err := s.createToken(ctx, "alice", identity, "Laptop", "write", []string{"acme/project", "alice/project"}, false, 30)
	if err != nil {
		t.Fatal(err)
	}
	username, id, ok := splitToken(raw)
	if !ok || username != "alice" || id != view.ID {
		t.Fatal("invalid token shape")
	}
	body, _, err := store.Get(ctx, tokenKey(username, id))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	if closeErr := body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(raw)) || bytes.Contains(data, []byte(strings.Split(raw, ".")[3])) || bytes.Contains(data, []byte(identity.Email)) {
		t.Fatal("token secret or email persisted")
	}
	if _, err := s.verifyLocalToken(ctx, "alice", raw); err != nil {
		t.Fatal("issued token rejected")
	}
	for _, test := range []struct{ name, username, raw string }{
		{"wrong username", "bob", raw},
		{"wrong secret", "alice", raw[:len(raw)-43] + strings.Repeat("A", 43)},
		{"truncated", "alice", raw[:len(raw)-1]},
		{"malformed", "alice", "gitone_pat_v1..."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.verifyLocalToken(ctx, test.username, test.raw); !errors.Is(err, errInvalidToken) {
				t.Fatalf("expected invalid token, got %v", err)
			}
		})
	}
	views, err := s.listTokens(ctx, "alice")
	if err != nil || len(views) != 1 || views[0].ID != id {
		t.Fatalf("list failed: %v", err)
	}
	for range 2 {
		if err := s.revokeToken(ctx, "alice", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.verifyLocalToken(ctx, "alice", raw); !errors.Is(err, errInvalidToken) {
		t.Fatal("revoked token accepted")
	}
}

func TestTokenRepositorySelectionPersistence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		all    bool
		scopes []string
	}{
		{name: "legacy selected repositories", scopes: []string{"alice/project"}},
		{name: "all current and future repositories", all: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := storage.NewMemoryStore()
			s := testService(t, 1, store, &fakeProvider{}, nil)
			identity := Identity{Subject: "alice-id"}
			if err := s.bindUser(ctx, "alice", identity); err != nil {
				t.Fatal(err)
			}
			raw, view, err := s.createToken(ctx, "alice", identity, "Laptop", "read", test.scopes, test.all, 1)
			if err != nil {
				t.Fatal(err)
			}
			if view.AllRepositories != test.all || view.Repositories == nil || len(view.Repositories) != len(test.scopes) {
				t.Fatal("wrong selection returned")
			}
			body, _, err := store.Get(ctx, tokenKey("alice", view.ID))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			if closeErr := body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(`"allRepositories"`)) != test.all {
				t.Fatal("selection flag does not preserve legacy record shape")
			}
			// A fresh service reads the persisted selection, rather than a cache.
			restarted := testService(t, 1, store, &fakeProvider{}, nil)
			record, err := restarted.verifyLocalToken(ctx, "alice", raw)
			if err != nil || record.Metadata.AllRepositories != test.all {
				t.Fatal("persisted selection not verified")
			}
			listed, err := restarted.listTokens(ctx, "alice")
			if err != nil || len(listed) != 1 || listed[0].AllRepositories != test.all {
				t.Fatal("listed selection differs")
			}
			if err := restarted.revokeToken(ctx, "alice", view.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.verifyLocalToken(ctx, "alice", raw); !errors.Is(err, errInvalidToken) {
				t.Fatal("revoked selection accepted")
			}
		})
	}
}

func TestTokenExpiryBindingAndCorruption(t *testing.T) {
	t.Parallel()
	for _, test := range []string{"expired", "identity changed", "corrupt record", "missing record", "wrong issuer"} {
		t.Run(test, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := storage.NewMemoryStore()
			s := testService(t, 1, store, &fakeProvider{}, nil)
			identity := Identity{Subject: "alice-id"}
			if err := s.bindUser(ctx, "alice", identity); err != nil {
				t.Fatal(err)
			}
			raw, view, err := s.createToken(ctx, "alice", identity, "Laptop", "read", []string{"alice/project"}, false, 1)
			if err != nil {
				t.Fatal(err)
			}
			record, version, err := s.loadToken(ctx, "alice", view.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch test {
			case "identity changed":
				record.Identity.Subject = "different"
			case "wrong issuer":
				record.Identity.Issuer = "https://other.example"
			case "expired":
				record.Metadata.CreatedAt = time.Now().Add(-48 * time.Hour)
				record.Metadata.ExpiresAt = time.Now().Add(-24 * time.Hour)
			case "missing record":
				if err := store.Delete(ctx, tokenKey("alice", view.ID), version); err != nil {
					t.Fatal(err)
				}
			}
			if test != "missing record" {
				data, _ := json.Marshal(record)
				if test == "corrupt record" {
					data = []byte(`{"digest":"invalid"}`)
				}
				if _, err := store.Put(ctx, tokenKey("alice", view.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.verifyLocalToken(ctx, "alice", raw); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
}

func TestTokenSettingsValidation(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, test := range []struct {
		name, permission string
		scopes           []string
		days             int
	}{
		{"", "read", []string{"alice/project"}, 30},
		{"name", "admin", []string{"alice/project"}, 30},
		{"name", "read", nil, 30},
		{"name", "read", []string{"*"}, 30},
		{"name", "read", []string{"alice/../project"}, 30},
		{"name", "read", []string{"auth/project"}, 30},
		{"name", "read", []string{"alice/project", "alice/project"}, 30},
		{"name", "read", []string{"alice/project"}, 0},
		{"name", "read", []string{"alice/project"}, 91},
	} {
		if _, _, err := s.createToken(context.Background(), "alice", Identity{Subject: "alice-id"}, test.name, test.permission, test.scopes, false, test.days); err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
}

func TestTokenScopeModes(t *testing.T) {
	t.Parallel()
	s := testService(t, 1, storage.NewMemoryStore(), &fakeProvider{}, nil)
	for _, test := range []struct {
		name   string
		all    bool
		scopes []string
		valid  bool
	}{
		{name: "all without repositories", all: true, valid: true},
		{name: "all with empty list", all: true, scopes: []string{}, valid: true},
		{name: "all with explicit repository rejected", all: true, scopes: []string{"alice/project"}},
		{name: "all with wildcard rejected", all: true, scopes: []string{"*"}},
		{name: "empty legacy scopes never broaden access"},
		{name: "empty selected scopes rejected", scopes: []string{}},
		{name: "selected exact repository", scopes: []string{"alice/project"}, valid: true},
		{name: "selected wildcard rejected", scopes: []string{"*"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := s.validTokenScopes(test.all, test.scopes); got != test.valid {
				t.Fatalf("valid=%v want=%v", got, test.valid)
			}
		})
	}
}

func TestTokenListingPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	s := testService(t, 1, store, &fakeProvider{}, nil)
	_, view, err := s.createToken(ctx, "alice", Identity{Subject: "alice-id"}, "Laptop", "read", []string{"alice/project"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	record, version, err := s.loadToken(ctx, "alice", view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, tokenKey("alice", view.ID), version); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 1001; i++ {
		record.Metadata.ID = fmt.Sprintf("%032x", i)
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, tokenKey("alice", record.Metadata.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfNoneMatch: true}); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := s.listTokenPage(ctx, "alice", "")
	if err != nil || len(first) != 1000 || cursor != fmt.Sprintf("%032x", 1000) {
		t.Fatalf("first page length=%d cursor=%s err=%v", len(first), cursor, err)
	}
	last, cursor, err := s.listTokenPage(ctx, "alice", cursor)
	if err != nil || len(last) != 1 || cursor != "" || last[0].ID != fmt.Sprintf("%032x", 1001) {
		t.Fatalf("last page length=%d err=%v", len(last), err)
	}
}
