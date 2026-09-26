package metrics_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestStoreMultipartOperations(t *testing.T) {
	t.Parallel()
	store := newStore(t, storage.NewMemoryStore())
	id, err := store.CreateMultipart(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	part, err := store.UploadPart(t.Context(), "key", id, 1, strings.NewReader("content"), 7)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.CompleteMultipart(t.Context(), "key", id, []storage.MultipartPart{part})
	if err != nil || info.Size != 7 {
		t.Fatalf("complete: %+v, %v", info, err)
	}
	if err := store.AbortMultipart(t.Context(), "key", id); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"create_multipart", "upload_part", "complete_multipart", "abort_multipart"} {
		assertSample(t, store, "gitone_storage_operations_total{operation=\""+op+"\",result=\"success\"}", 1)
		assertSample(t, store, "gitone_storage_active_operations{operation=\""+op+"\"}", 0)
	}
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="upload_part"}`, 7)
}

func TestStoreMultipartRetriesAndUnsupported(t *testing.T) {
	t.Parallel()
	inner := &multipartMetricsStore{MemoryStore: storage.NewMemoryStore()}
	store := newStore(t, inner)
	if _, err := store.UploadPart(t.Context(), "key", "upload", 1, strings.NewReader("retry"), 5); err != nil {
		t.Fatal(err)
	}
	assertSample(t, store, `gitone_storage_transferred_bytes_total{operation="upload_part"}`, 10)
	unsupported := newStore(t, struct{ storage.ObjectStore }{storage.NewMemoryStore()})
	if _, err := unsupported.CreateMultipart(t.Context(), "key"); !errors.Is(err, storage.ErrMultipartUnsupported) {
		t.Fatalf("create: %v", err)
	}
	if _, err := unsupported.UploadPart(t.Context(), "key", "upload", 1, strings.NewReader("x"), 1); !errors.Is(err, storage.ErrMultipartUnsupported) {
		t.Fatalf("part: %v", err)
	}
	if _, err := unsupported.CompleteMultipart(t.Context(), "key", "upload", nil); !errors.Is(err, storage.ErrMultipartUnsupported) {
		t.Fatalf("complete: %v", err)
	}
	if err := unsupported.AbortMultipart(t.Context(), "key", "upload"); !errors.Is(err, storage.ErrMultipartUnsupported) {
		t.Fatalf("abort: %v", err)
	}
	for _, op := range []string{"create_multipart", "upload_part", "complete_multipart", "abort_multipart"} {
		assertSample(t, unsupported, "gitone_storage_operations_total{operation=\""+op+"\",result=\"error\"}", 1)
		assertSample(t, unsupported, "gitone_storage_active_operations{operation=\""+op+"\"}", 0)
	}
}

type multipartMetricsStore struct{ *storage.MemoryStore }

func (s *multipartMetricsStore) UploadPart(_ context.Context, _ string, _ string, number int, body io.ReadSeeker, size int64) (storage.MultipartPart, error) {
	for range 2 {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return storage.MultipartPart{}, err
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return storage.MultipartPart{}, err
		}
	}
	return storage.MultipartPart{Number: number, ETag: "etag", Size: size}, nil
}
