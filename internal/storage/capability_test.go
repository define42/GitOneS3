package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestVerifyConditionalOperations(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	if err := VerifyConditionalOperations(
		context.Background(),
		store,
		"maintenance/capabilities/",
	); err != nil {
		t.Fatalf("VerifyConditionalOperations() error = %v", err)
	}
	objects, err := store.List(context.Background(), "maintenance/capabilities/")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("capability probe left %d objects behind", len(objects))
	}
}

func TestVerifyConditionalOperationsRejectsIgnoredConditions(t *testing.T) {
	t.Parallel()

	store := &unconditionalStore{MemoryStore: NewMemoryStore()}
	err := VerifyConditionalOperations(
		context.Background(),
		store,
		"maintenance/capabilities/",
	)
	if !errors.Is(err, ErrConditionalUnsupported) {
		t.Fatalf("VerifyConditionalOperations() error = %v, want unsupported", err)
	}
}

func TestVerifyConditionalOperationsRejectsIgnoredDeleteConditions(t *testing.T) {
	t.Parallel()

	store := &unconditionalDeleteStore{MemoryStore: NewMemoryStore()}
	err := VerifyConditionalOperations(t.Context(), store, "maintenance/capabilities/")
	if !errors.Is(err, ErrConditionalUnsupported) {
		t.Fatalf("VerifyConditionalOperations() error = %v, want unsupported", err)
	}
}

type unconditionalDeleteStore struct {
	*MemoryStore
}

func (s *unconditionalDeleteStore) Delete(ctx context.Context, key string, _ Version) error {
	return s.MemoryStore.Delete(ctx, key, "")
}

type unconditionalStore struct {
	*MemoryStore
}

func (s *unconditionalStore) Put(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
	_ PutOptions,
) (ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	return s.MemoryStore.Put(ctx, key, bytes.NewReader(data), size, PutOptions{})
}

func (s *unconditionalStore) Delete(ctx context.Context, key string, _ Version) error {
	return s.MemoryStore.Delete(ctx, key, "")
}
