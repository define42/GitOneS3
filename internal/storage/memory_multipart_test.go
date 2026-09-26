package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestMemoryMultipartVisibilityRetryAndCompletion(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	const key = "repos/example/lfs/data/upload"
	id, err := store.CreateMultipart(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.UploadPart(t.Context(), key, id, 1, strings.NewReader("old"), 3)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("a"), int(storage.MinMultipartPartSize))
	first, err := store.UploadPart(t.Context(), key, id, 1, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	last, err := store.UploadPart(t.Context(), key, id, 2, strings.NewReader("end"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("incomplete object visible: %v", err)
	}
	if page, err := store.List(t.Context(), "repos/"); err != nil || len(page) != 0 {
		t.Fatalf("incomplete listed: %v, %v", page, err)
	}
	if _, err := store.CompleteMultipart(t.Context(), key, id, []storage.MultipartPart{stale}); !errors.Is(err, storage.ErrInvalidMultipart) {
		t.Fatalf("stale completion: %v", err)
	}
	info, err := store.CompleteMultipart(t.Context(), key, id, []storage.MultipartPart{first, last})
	if err != nil {
		t.Fatal(err)
	}
	body, got, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	if got != info || info.Version == "" || !bytes.Equal(data, append(payload, []byte("end")...)) {
		t.Fatal("completed content or metadata differs")
	}
	if _, err := store.UploadPart(t.Context(), key, id, 1, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("completed upload accepted another part: %v", err)
	}
	if err := store.AbortMultipart(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(t.Context(), key); err != nil {
		t.Fatalf("abort deleted completed object: %v", err)
	}
}

func TestMemoryMultipartAbortAndValidation(t *testing.T) {
	t.Parallel()
	store := storage.NewMemoryStore()
	id, err := store.CreateMultipart(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadPart(t.Context(), "other", id, 1, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-key upload: %v", err)
	}
	for _, test := range []struct {
		name, data string
		size       int64
		number     int
	}{
		{name: "truncated", data: "x", size: 2, number: 1},
		{name: "excess", data: "xx", size: 1, number: 1},
		{name: "negative", size: -1, number: 1},
		{name: "large", size: storage.MaxMultipartPartSize + 1, number: 1},
		{name: "zero part", number: 0},
		{name: "large part number", number: storage.MaxMultipartParts + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.UploadPart(t.Context(), "key", id, test.number, strings.NewReader(test.data), test.size); err == nil {
				t.Fatal("invalid part accepted")
			}
		})
	}
	if err := store.AbortMultipart(t.Context(), "key", id); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortMultipart(t.Context(), "key", id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UploadPart(t.Context(), "key", id, 1, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("aborted upload accepted part: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.CreateMultipart(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("create canceled: %v", err)
	}
	if _, err := store.UploadPart(ctx, "key", id, 1, strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload canceled: %v", err)
	}
	if _, err := store.CompleteMultipart(ctx, "key", id, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("complete canceled: %v", err)
	}
	if err := store.AbortMultipart(ctx, "key", id); !errors.Is(err, context.Canceled) {
		t.Fatalf("abort canceled: %v", err)
	}
}
