package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func testHandler(t *testing.T) (*Handler, *repository.Store) {
	t.Helper()
	store, err := repository.New(storage.NewMemoryStore())
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
	return handler, store
}

func request(t *testing.T, h http.Handler, method, path string, data []byte, write bool) *httptest.ResponseRecorder {
	t.Helper()
	ctx := t.Context()
	if write {
		ctx = gittransport.WithWriteAuthorization(ctx, func(context.Context) error { return nil })
	}
	r := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", mediaType+"; charset=utf-8")
	if method == http.MethodPut {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func objectID(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func TestHandlerLifecycleWithoutTemporaryDisk(t *testing.T) {
	// This test runs before parallel tests and makes any server disk staging fail.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "nonexistent"))
	handler, store := testHandler(t)
	data := bytes.Repeat([]byte("streamed LFS\n"), 800000)
	oid := objectID(data)
	batchBody := []byte(fmt.Sprintf(`{"operation":"upload","objects":[{"oid":%q,"size":%d}]}`, oid, len(data)))
	w := request(t, handler, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", batchBody, true)
	if w.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", w.Code, w.Body)
	}
	var batch batchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Objects) != 1 || batch.Objects[0].Error != nil || len(batch.Objects[0].Actions) != 2 {
		t.Fatalf("upload actions: %+v", batch)
	}
	for _, action := range batch.Objects[0].Actions {
		if !strings.HasPrefix(action.Href, "https://gitone.example/alice/demo.git/info/lfs/") {
			t.Fatalf("external action URL: %q", action.Href)
		}
	}
	uploadPath := strings.TrimPrefix(batch.Objects[0].Actions["upload"].Href, "https://gitone.example")
	w = request(t, handler, http.MethodPut, uploadPath, data, true)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if object, err := store.LFSStat(t.Context(), "alice", "demo", oid); err != nil || object.Size != int64(len(data)) {
		t.Fatalf("stat: %+v %v", object, err)
	}
	w = request(t, handler, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", batchBody, true)
	batch = batchResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Objects[0].Actions) != 0 {
		t.Fatalf("deduplicated upload has actions: %s", w.Body)
	}
	verify := []byte(fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, len(data)))
	w = request(t, handler, http.MethodPost, "/alice/demo.git/info/lfs/objects/"+oid+"/verify", verify, true)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	w = request(t, handler, http.MethodGet, "/alice/demo.git/info/lfs/objects/"+oid, nil, false)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download: %d, %d bytes", w.Code, w.Body.Len())
	}
	w = request(t, handler, http.MethodHead, "/alice/demo.git/info/lfs/objects/"+oid, nil, false)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
		t.Fatalf("head: %d %s", w.Code, w.Body)
	}
}

func TestHandlerDownloadRanges(t *testing.T) {
	t.Parallel()
	handler, _ := testHandler(t)
	data := []byte("0123456789")
	oid := objectID(data)
	path := "/alice/demo.git/info/lfs/objects/" + oid
	if w := request(t, handler, http.MethodPut, path+"?size=10", data, true); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	for _, test := range []struct {
		name, value, want, contentRange string
		status                          int
	}{
		{"middle", "bytes=2-5", "2345", "bytes 2-5/10", 206},
		{"open", "bytes=7-", "789", "bytes 7-9/10", 206},
		{"suffix", "bytes=-3", "789", "bytes 7-9/10", 206},
		{"clamped", "bytes=8-99", "89", "bytes 8-9/10", 206},
		{"past end", "bytes=10-", "", "bytes */10", 416},
		{"multiple", "bytes=0-1,4-5", "", "bytes */10", 416},
		{"negative", "bytes=-0", "", "bytes */10", 416},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			r.Header.Set("Range", test.value)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status || w.Header().Get("Content-Range") != test.contentRange || (test.status == 206 && w.Body.String() != test.want) {
				t.Fatalf("range: %d %s %q", w.Code, w.Header().Get("Content-Range"), w.Body)
			}
		})
	}
	for _, test := range []struct {
		name, header, value string
		status              int
	}{
		{"cached", "If-None-Match", `"` + oid + `"`, 304},
		{"weak cached", "If-None-Match", `W/"` + oid + `"`, 304},
		{"stale range", "If-Range", `"old"`, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			r.Header.Set("Range", "bytes=0-1")
			r.Header.Set(test.header, test.value)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("conditional download: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestHandlerRejectsInvalidUploads(t *testing.T) {
	t.Parallel()
	handler, store := testHandler(t)
	oid := objectID([]byte("good"))
	path := "/alice/demo.git/info/lfs/objects/" + oid
	for _, test := range []struct {
		name, suffix string
		body         []byte
		write        bool
		status       int
	}{
		{"unauthorized", "?size=4", []byte("good"), false, 403},
		{"wrong hash", "?size=4", []byte("evil"), true, 422},
		{"wrong length", "?size=3", []byte("good"), true, 422},
		{"no size", "", []byte("good"), true, 400},
		{"negative", "?size=-1", nil, true, 400},
		{"over limit", "?size=1073741825", nil, true, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := request(t, handler, http.MethodPut, path+test.suffix, test.body, test.write)
			if w.Code != test.status {
				t.Fatalf("upload: %d %s", w.Code, w.Body)
			}
		})
	}
	if _, err := store.LFSStat(t.Context(), "alice", "demo", oid); err == nil {
		t.Fatal("rejected upload became visible")
	}
}

func TestHandlerBatchValidationAndGrantActions(t *testing.T) {
	t.Parallel()
	handler, _ := testHandler(t)
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"download miss", `{"operation":"download","objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]}`, 200},
		{"empty", `{"operation":"download","objects":[]}`, 200},
		{"unknown operation", `{"operation":"delete","objects":[]}`, 400},
		{"case operation", `{"Operation":"download","objects":[]}`, 400},
		{"trailing", `{"operation":"download","objects":[]} {}`, 400},
		{"adapter", `{"operation":"download","transfers":["custom"],"objects":[]}`, 422},
		{"hash", `{"operation":"download","hash_algo":"sha1","objects":[]}`, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := request(t, handler, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", []byte(test.body), true)
			if w.Code != test.status {
				t.Fatalf("batch: %d %s", w.Code, w.Body)
			}
		})
	}
	ctx := gittransport.WithWriteAuthorization(t.Context(), func(context.Context) error { return nil })
	expires := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	ctx = gittransport.WithLFSCredentialExpiry(ctx, expires)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/alice/demo.git/info/lfs/objects/batch", strings.NewReader(`{"operation":"upload","objects":[{"oid":"`+strings.Repeat("b", 64)+`","size":8}]}`))
	r.Header.Set("Content-Type", mediaType)
	r.Header.Set("Authorization", "Bearer short-lived-test-grant")
	r.Host = "attacker.invalid"
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var batch batchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Objects) != 1 || !batch.Objects[0].Authenticated {
		t.Fatalf("grant batch: %s", w.Body)
	}
	if len(batch.Objects[0].Actions) != 1 || batch.Objects[0].Actions["upload"].Href == "" {
		t.Fatalf("SSH upload must omit optional verify: %s", w.Body)
	}
	for _, action := range batch.Objects[0].Actions {
		if action.Header["Authorization"] != r.Header.Get("Authorization") || !strings.HasPrefix(action.Href, "https://gitone.example/") ||
			action.ExpiresAt == nil || !action.ExpiresAt.Equal(expires) {
			t.Fatalf("grant action: %+v", action)
		}
	}
}

func TestHandlerFileLockingUnsupported(t *testing.T) {
	t.Parallel()
	handler, _ := testHandler(t)
	for _, test := range []struct{ name, method, suffix string }{
		{name: "list", method: http.MethodGet, suffix: "/locks"},
		{name: "create", method: http.MethodPost, suffix: "/locks"},
		{name: "verify", method: http.MethodPost, suffix: "/locks/verify"},
		{name: "unlock", method: http.MethodPost, suffix: "/locks/123/unlock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := request(t, handler, test.method, "/alice/demo.git/info/lfs"+test.suffix, nil, true)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("unsupported locking endpoint: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandlerAdmissionCancellation(t *testing.T) {
	t.Parallel()
	handler, _ := testHandler(t)
	handler.active = make(chan struct{}, 1)
	handler.waiting = make(chan struct{}, 1)
	handler.options.QueueTimeout = time.Second
	release, err := handler.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := handler.acquire(ctx); err == nil {
		t.Fatal("canceled waiter admitted")
	}
	if len(handler.waiting) != 0 {
		t.Fatal("canceled request leaked a queue slot")
	}
}
