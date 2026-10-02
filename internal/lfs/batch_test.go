package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestBatchObjectOrderAndErrors(t *testing.T) {
	t.Parallel()
	handler, store := testHandler(t)
	data := []byte("existing object")
	existing := repository.LFSObject{OID: objectID(data), Size: int64(len(data))}
	if _, err := store.LFSUpload(t.Context(), "alice", "demo", existing.OID, existing.Size, bytes.NewReader(data),
		repository.LFSLimits{MaxObjectBytes: 1024, MaxRepositoryBytes: 2048}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	missing := repository.LFSObject{OID: strings.Repeat("a", 64), Size: 7}
	objects := []repository.LFSObject{
		existing,
		{OID: "invalid", Size: 1},
		missing,
		{OID: existing.OID, Size: existing.Size + 1},
		{OID: strings.Repeat("b", 64), Size: -1},
		existing,
		{OID: strings.Repeat("c", 64), Size: handler.options.MaxObjectBytes + 1},
	}
	for _, operation := range []string{"upload", "download"} {
		t.Run(operation, func(t *testing.T) {
			body, err := json.Marshal(batchRequest{Operation: operation, Objects: objects})
			if err != nil {
				t.Fatal(err)
			}
			w := request(t, handler, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", body, operation == "upload")
			if w.Code != http.StatusOK {
				t.Fatalf("batch = %d %s", w.Code, w.Body)
			}
			var response batchResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Objects) != len(objects) {
				t.Fatalf("objects = %+v", response.Objects)
			}
			codes := []int{0, 422, 0, 422, 422, 0, 413}
			if operation == "download" {
				codes[2], codes[6] = 404, 404
			}
			for i, result := range response.Objects {
				if result.OID != objects[i].OID || result.Size != objects[i].Size {
					t.Errorf("object %d changed order: %+v", i, result)
				}
				if codes[i] != 0 {
					if result.Error == nil || result.Error.Code != codes[i] || len(result.Actions) != 0 {
						t.Errorf("object %d = %+v, want error %d", i, result, codes[i])
					}
					continue
				}
				if result.Error != nil {
					t.Fatalf("object %d = %+v", i, result)
				}
				if operation == "download" && result.Actions["download"].Href == "" {
					t.Errorf("object %d has no download action", i)
				}
				if operation == "upload" && ((i == 2 && len(result.Actions) != 2) || (i != 2 && len(result.Actions) != 0)) {
					t.Errorf("object %d upload actions = %+v", i, result.Actions)
				}
			}
		})
	}
}

func TestBatchAuthorizationAndCancellation(t *testing.T) {
	t.Parallel()
	objects := storage.NewMemoryStore()
	observed := &batchBlockingStore{ObjectStore: objects}
	store, err := repository.New(observed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "demo", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store, Options{PublicURL: "https://gitone.example"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"operation":"upload","objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]}`
	ctx := gittransport.WithWriteAuthorization(t.Context(), func(context.Context) error { return errors.New("revoked") })
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", strings.NewReader(body))
	r.Header.Set("Content-Type", mediaType)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || observed.started.Load() != 0 {
		t.Fatalf("revoked upload = %d, lookups = %d", w.Code, observed.started.Load())
	}
	synctest.Test(t, func(t *testing.T) {
		body := `{"operation":"download","objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]}`
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", strings.NewReader(body))
		r.Header.Set("Content-Type", mediaType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusGatewayTimeout || observed.started.Load() != 1 || observed.active.Load() != 0 {
			t.Fatalf("expired batch = %d, lookups = %d, active = %d", w.Code, observed.started.Load(), observed.active.Load())
		}
		if len(handler.control) != 0 {
			t.Fatal("cancelled batch leaked control admission")
		}
	})
}

type batchBlockingStore struct {
	storage.ObjectStore
	started atomic.Int64
	active  atomic.Int64
}

func (s *batchBlockingStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/lfs/verified/") {
		s.started.Add(1)
		s.active.Add(1)
		defer s.active.Add(-1)
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, storage.ObjectInfo{}, ctx.Err()
		case <-timer.C:
		}
	}
	return s.ObjectStore.Get(ctx, key)
}
