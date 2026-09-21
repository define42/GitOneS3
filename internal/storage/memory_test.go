package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMemoryStore_PutConditions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	first, err := store.Put(ctx, "packs/a.pack", bytes.NewBufferString("one"), 3, PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatalf("initial Put() error = %v", err)
	}
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfNoneMatch: true},
	); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("immutable overwrite error = %v, want ErrAlreadyExists", err)
	}
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfMatch: "wrong"},
	); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("wrong version error = %v, want ErrPreconditionFailed", err)
	}
	second, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("two"),
		3,
		PutOptions{IfMatch: first.Version},
	)
	if err != nil {
		t.Fatalf("conditional Put() error = %v", err)
	}
	if second.Version == first.Version {
		t.Fatal("conditional Put() did not advance the version")
	}
}

func TestMemoryStore_GetRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryStore()
	if _, err := store.Put(
		ctx,
		"packs/a.pack",
		bytes.NewBufferString("0123456789"),
		10,
		PutOptions{},
	); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	body, _, err := store.GetRange(ctx, "packs/a.pack", 3, 4)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := string(data), "3456"; got != want {
		t.Fatalf("GetRange() = %q, want %q", got, want)
	}
}

func TestMemoryStore_RejectsSizeMismatch(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	_, err := store.Put(
		context.Background(),
		"packs/a.pack",
		bytes.NewBufferString("short"),
		10,
		PutOptions{},
	)
	if err == nil {
		t.Fatal("Put() error = nil, want size mismatch")
	}
}
