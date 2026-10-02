package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestBrowserAdmissionBoundsAllManifestRoutes(t *testing.T) {
	t.Parallel()
	s, objects, cookie := browserAdmissionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := "/api/v1/repos/alice/demo"
	completed := make(chan *httptest.ResponseRecorder, maxBrowserReads)
	objects.blocked.Store(true)
	for _, suffix := range []string{"", "/branches", "/tree", "/blob?path=README.md"} {
		go func() {
			completed <- browserAdmissionRequest(ctx, s, cookie, base+suffix)
		}()
	}
	for range maxBrowserReads {
		select {
		case <-objects.started:
		case <-time.After(5 * time.Second):
			t.Fatal("browser request did not start its manifest read")
		}
	}
	for _, suffix := range []string{"", "/branches", "/tree", "/blob?path=README.md", "/commits", "/browse"} {
		w := browserAdmissionRequest(ctx, s, cookie, base+suffix)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("full browser gate %s = %d, headers %v", suffix, w.Code, w.Header())
		}
	}
	if got := objects.reads.Load(); got != maxBrowserReads {
		t.Fatalf("rejected requests loaded manifests: %d", got)
	}
	// Lightweight catalog pagination remains independent of manifest admission.
	if w := browserAdmissionRequest(ctx, s, cookie, "/api/v1/repos/alice"); w.Code != http.StatusOK {
		t.Fatalf("catalog during browser saturation = %d %s", w.Code, w.Body)
	}
	close(objects.resume)
	for range maxBrowserReads {
		select {
		case w := <-completed:
			if w.Code != http.StatusOK {
				t.Errorf("admitted browser request = %d %s", w.Code, w.Body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("browser request did not release its admission")
		}
	}
	if len(s.browserReads) != 0 {
		t.Fatal("completed browser requests leaked slots")
	}
	if w := browserAdmissionRequest(ctx, s, cookie, base+"/browse?path=missing"); w.Code != http.StatusNotFound || len(s.browserReads) != 0 {
		t.Fatalf("failed browse = %d, active = %d", w.Code, len(s.browserReads))
	}
	if w := browserAdmissionRequest(ctx, s, cookie, base+"/browse"); w.Code != http.StatusOK {
		t.Fatalf("browser after admission release = %d %s", w.Code, w.Body)
	}
}

func TestBrowserAdmissionCancellationReleasesSlot(t *testing.T) {
	t.Parallel()
	s, objects, cookie := browserAdmissionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	objects.blocked.Store(true)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		completed <- browserAdmissionRequest(ctx, s, cookie, "/api/v1/repos/alice/demo/browse")
	}()
	select {
	case <-objects.started:
	case <-time.After(5 * time.Second):
		t.Fatal("browser did not start its manifest read")
	}
	cancel()
	select {
	case w := <-completed:
		if w.Code != http.StatusServiceUnavailable || len(s.browserReads) != 0 {
			t.Fatalf("cancelled browse = %d, active = %d", w.Code, len(s.browserReads))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled browser did not return")
	}
	if release, err := s.acquireBrowser(ctx); release != nil || err == nil || len(s.browserReads) != 0 {
		t.Fatalf("cancelled request admitted: error = %v, active = %d", err, len(s.browserReads))
	}
}

func browserAdmissionFixture(t *testing.T) (*Service, *browserAdmissionStore, *http.Cookie) {
	t.Helper()
	objects := &browserAdmissionStore{
		ObjectStore: storage.NewMemoryStore(), started: make(chan struct{}, maxBrowserReads), resume: make(chan struct{}),
	}
	s := testService(t, 1, objects, &fakeProvider{}, nil)
	if err := s.bindUser(t.Context(), "alice", Identity{Subject: "alice-id"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repositories.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "alice-id", InitializeReadme: true, AuthorName: "Alice", AuthorEmail: "alice@users.gitone.invalid",
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := groupSession(t, s, "alice", "alice-id")
	return s, objects, cookie
}

func browserAdmissionRequest(ctx context.Context, s *Service, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

type browserAdmissionStore struct {
	storage.ObjectStore
	blocked atomic.Bool
	reads   atomic.Int64
	started chan struct{}
	resume  chan struct{}
}

func (s *browserAdmissionStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "-manifest-") {
		s.reads.Add(1)
		if s.blocked.Load() {
			s.started <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, storage.ObjectInfo{}, ctx.Err()
			case <-s.resume:
			}
		}
	}
	return s.ObjectStore.Get(ctx, key)
}
