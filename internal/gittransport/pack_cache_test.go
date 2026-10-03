package gittransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestPreparedPackReuseHTTPAndSSH(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	handler, objects, shared := preparedPackFixture(t, fixture, 4<<30)
	wants := []string{ids["merge"]}
	expected := preparedPackExpected(t, fixture, wants, nil)
	checkPreparedPack(t, preparedPackHTTP(t, handler, wants, nil, false), false, expected)
	reads := objects.payloadReads.Load()
	misses := packCacheMetric(t, shared, "disk_builds_total")
	for _, test := range []struct {
		name     string
		ssh      bool
		sideband bool
	}{
		{name: "HTTP raw"},
		{name: "HTTP sideband", sideband: true},
		{name: "SSH raw", ssh: true},
		{name: "SSH sideband", ssh: true, sideband: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response []byte
			if test.ssh {
				stream := &sshTestStream{input: bytes.NewReader(uploadRequest(wants, nil, test.sideband, true))}
				if err := handler.ServeSSH(t.Context(), SSHRequest{
					Namespace: "alice", Repository: "cached", Service: upload, Stream: stream,
				}); err != nil {
					t.Fatal(err)
				}
				reader := bytes.NewReader(stream.output.Bytes())
				for {
					_, flush, err := readPkt(reader)
					if err != nil {
						t.Fatal(err)
					}
					if flush {
						break
					}
				}
				response = stream.output.Bytes()[stream.output.Len()-reader.Len():]
			} else {
				response = preparedPackHTTP(t, handler, wants, nil, test.sideband)
			}
			checkPreparedPack(t, response, test.sideband, expected)
			if got := objects.payloadReads.Load(); got != reads {
				t.Fatalf("warm clone reread object storage: %d -> %d", reads, got)
			}
			if got := packCacheMetric(t, shared, "disk_builds_total"); got != misses {
				t.Fatalf("warm clone rebuilt a disk entry: %d -> %d", misses, got)
			}
			if pins := packCacheMetric(t, shared, "disk_pins"); pins != 0 {
				t.Fatalf("completed transfer retained %d file pins", pins)
			}
		})
	}
}

func TestPreparedPackSelectionAndPublication(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	handler, _, shared := preparedPackFixture(t, fixture, 4<<30)
	for _, test := range []struct {
		name         string
		wants, haves []string
	}{
		{name: "main", wants: []string{ids["merge"]}},
		{name: "other branch", wants: []string{ids["left"]}},
		{name: "tag object", wants: []string{ids["nested-tag"]}},
		{name: "incremental", wants: []string{ids["merge"]}, haves: []string{ids["left"]}},
		{name: "reordered wants", wants: []string{ids["left"], ids["right"]}},
		{name: "same reordered selection", wants: []string{ids["right"], ids["left"]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkPreparedPack(t, preparedPackHTTP(t, handler, test.wants, test.haves, true), true,
				preparedPackExpected(t, fixture, test.wants, test.haves))
		})
	}
	before := packCacheMetric(t, shared, "disk_builds_total")
	base, err := handler.store.ReadGitReferences(t.Context(), "alice", "cached")
	if err != nil {
		t.Fatal(err)
	}
	commit := fixture.Objects[ids["merge"]]
	commit.Data = append(bytes.Clone(commit.Data), []byte("new generation\n")...)
	id := repository.GitObjectID(commit)
	if err := handler.store.PublishGit(t.Context(), base,
		[]repository.RefUpdate{{Name: "refs/heads/main", Old: ids["merge"], New: id}},
		map[string]repository.GitObject{id: commit}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fixture.Objects[id] = commit
	fixture.References["refs/heads/main"] = id
	wants := []string{id}
	checkPreparedPack(t, preparedPackHTTP(t, handler, wants, nil, false), false,
		preparedPackExpected(t, fixture, wants, nil))
	if after := packCacheMetric(t, shared, "disk_builds_total"); after <= before {
		t.Fatal("new generation reused an earlier prepared pack")
	}
	// A cached old pack cannot make a no-longer-advertised want acceptable.
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/cached.git/git-upload-pack",
		bytes.NewReader(uploadRequest([]string{ids["merge"]}, nil, false, true)))
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("old unadvertised want status = %d", response.Code)
	}
}

func TestPreparedPackConcurrentClones(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	handler, _, shared := preparedPackFixture(t, fixture, 4<<30)
	// Populate the repository caches without generating an outgoing pack. This
	// isolates the shared clone fill from graph/metadata fill metrics.
	snapshot, err := handler.store.ReadGitReferences(t.Context(), "alice", "cached")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := handler.store.OpenGit(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	selected, err := reader.Reachable(t.Context(), map[string]string{"refs/heads/main": ids["merge"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Prefetch(t.Context(), selected); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	before := packCacheMetric(t, shared, "disk_builds_total")
	const clients = 8
	responses := make([][]byte, clients)
	statuses := make([]int, clients)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := range clients {
		workers.Go(func() {
			<-start
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/cached.git/git-upload-pack",
				bytes.NewReader(uploadRequest([]string{ids["merge"]}, nil, false, true)))
			request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses[i], statuses[i] = response.Body.Bytes(), response.Code
		})
	}
	close(start)
	workers.Wait()
	expected := preparedPackExpected(t, fixture, []string{ids["merge"]}, nil)
	for i := range clients {
		if statuses[i] != http.StatusOK {
			t.Fatalf("client %d status = %d", i, statuses[i])
		}
		checkPreparedPack(t, responses[i], false, expected)
	}
	if after := packCacheMetric(t, shared, "disk_builds_total"); after-before != 1 {
		t.Fatalf("concurrent clones built %d cache files; want 1", after-before)
	}
	if pins := packCacheMetric(t, shared, "disk_pins"); pins != 0 {
		t.Fatalf("concurrent clones leaked %d pins", pins)
	}
}

func TestPreparedPackCapacityFallback(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	handler, _, shared := preparedPackFixture(t, fixture, 4096)
	wants := []string{ids["merge"]}
	expected := preparedPackExpected(t, fixture, wants, nil)
	for range 2 {
		checkPreparedPack(t, preparedPackHTTP(t, handler, wants, nil, true), true, expected)
	}
	if pins := packCacheMetric(t, shared, "disk_pins"); pins != 0 {
		t.Fatalf("fallback retained %d pins", pins)
	}
}

func TestPreparedPackSmallReservation(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	for _, test := range []struct {
		name      string
		diskBytes int64
		retain    bool
	}{
		{name: "small cache", diskBytes: 128 << 10},
		{name: "preserves other entries", diskBytes: repository.MaxPackBytes + 4096, retain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, objects, shared := preparedPackFixture(t, fixture, test.diskBytes)
			if test.retain {
				// This entry would be evicted by reserving the global 1 GiB pack
				// limit, even though the clone response is only a few KiB.
				body := bytes.Repeat([]byte("x"), 2<<20)
				file, err := shared.LoadFile(t.Context(), "unrelated", int64(len(body)), func(_ context.Context, out io.Writer) error {
					_, err := out.Write(body)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			wants := []string{ids["merge"]}
			expected := preparedPackExpected(t, fixture, wants, nil)
			checkPreparedPack(t, preparedPackHTTP(t, handler, wants, nil, false), false, expected)
			reads := objects.payloadReads.Load()
			builds := packCacheMetric(t, shared, "disk_builds_total")
			hits := packCacheMetric(t, shared, "disk_hits_total")
			checkPreparedPack(t, preparedPackHTTP(t, handler, wants, nil, false), false, expected)
			if objects.payloadReads.Load() != reads || packCacheMetric(t, shared, "disk_builds_total") != builds {
				t.Fatal("small clone was rebuilt instead of reused")
			}
			if packCacheMetric(t, shared, "disk_hits_total") <= hits {
				t.Fatal("small clone did not use its cached pack")
			}
			snapshot, err := handler.store.ReadGitReferences(t.Context(), "alice", "cached")
			if err != nil {
				t.Fatal(err)
			}
			response, err := handler.prepareUpload(t.Context(), snapshot, uploadRequest(wants, nil, false, true))
			if err != nil {
				t.Fatal(err)
			}
			_, pinned := response.body.(*cache.File)
			if err := response.Close(); err != nil {
				t.Fatal(err)
			}
			if !pinned {
				t.Fatal("small clone generated a private response instead of retaining a shared pack")
			}
			if test.retain {
				file, err := shared.OpenFile(t.Context(), "unrelated")
				if err != nil {
					t.Fatalf("small clone evicted unrelated entry: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreparedPackFailedSendReleasesPin(t *testing.T) {
	t.Parallel()
	fixture, ids := uploadFixture(t)
	handler, _, shared := preparedPackFixture(t, fixture, 4<<30)
	wants := []string{ids["merge"]}
	preparedPackHTTP(t, handler, wants, nil, false)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/cached.git/git-upload-pack",
		bytes.NewReader(uploadRequest(wants, nil, true, true)))
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	out := &failedPackResponse{header: make(http.Header)}
	handler.ServeHTTP(out, request)
	if out.writes != 1 {
		t.Fatalf("failed transfer writes = %d", out.writes)
	}
	if pins := packCacheMetric(t, shared, "disk_pins"); pins != 0 {
		t.Fatalf("failed transfer leaked %d pins", pins)
	}
	snapshot, err := handler.store.ReadGitReferences(t.Context(), "alice", "cached")
	if err != nil {
		t.Fatal(err)
	}
	response, err := handler.prepareUpload(t.Context(), snapshot, uploadRequest(wants, nil, false, true))
	if err != nil {
		t.Fatal(err)
	}
	if pins := packCacheMetric(t, shared, "disk_pins"); pins != 1 {
		_ = response.Close()
		t.Fatalf("prepared response retained %d pins; want 1", pins)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	writeErr := response.write(ctx, io.Discard)
	closeErr := response.Close()
	if !errors.Is(writeErr, context.Canceled) || closeErr != nil {
		t.Fatalf("cancelled send: write=%v close=%v", writeErr, closeErr)
	}
	if pins := packCacheMetric(t, shared, "disk_pins"); pins != 0 {
		t.Fatalf("cancelled transfer leaked %d pins", pins)
	}
}

type failedPackResponse struct {
	header http.Header
	writes int
}

func (w *failedPackResponse) Header() http.Header { return w.header }
func (w *failedPackResponse) WriteHeader(int)     {}
func (w *failedPackResponse) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func preparedPackFixture(t testing.TB, fixture *repository.GitSnapshot, diskBytes int64) (*Handler, *preparedPackReadStore, *cache.Cache) {
	t.Helper()
	shared, err := cache.New(cache.Options{MemoryBytes: 64 << 20, DiskBytes: diskBytes, Directory: t.TempDir(), Namespace: "prepared-pack-tests"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shared.Close(); err != nil {
			t.Error(err)
		}
	})
	objects := &preparedPackReadStore{ObjectStore: storage.NewMemoryStore()}
	store, err := repository.New(objects, repository.WithCache(shared))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "cached", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "cached")
	if err != nil {
		t.Fatal(err)
	}
	updates := make([]repository.RefUpdate, 0, len(fixture.References))
	for ref, id := range fixture.References {
		updates = append(updates, repository.RefUpdate{Name: ref, New: id})
	}
	if err := store.PublishGit(t.Context(), base, updates, fixture.Objects, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store, Options{MaxConcurrentOperations: 8})
	if err != nil {
		t.Fatal(err)
	}
	return handler, objects, shared
}

type preparedPackReadStore struct {
	storage.ObjectStore
	payloadReads atomic.Int64
}

func (s *preparedPackReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/packs/") || strings.Contains(key, "/objects/") {
		s.payloadReads.Add(1)
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *preparedPackReadStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/packs/") || strings.Contains(key, "/objects/") {
		s.payloadReads.Add(1)
	}
	return s.ObjectStore.GetRange(ctx, key, offset, length)
}

func preparedPackHTTP(t testing.TB, handler *Handler, wants, haves []string, sideband bool) []byte {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/cached.git/git-upload-pack",
		bytes.NewReader(uploadRequest(wants, haves, sideband, true)))
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("clone status = %d: %s", response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

func preparedPackExpected(t *testing.T, fixture *repository.GitSnapshot, wants, haves []string) map[string]repository.GitObject {
	t.Helper()
	walk := func(ids []string) map[string]repository.GitObject {
		refs := make(map[string]string, len(ids))
		for _, id := range ids {
			refs["refs/tags/"+id] = id
		}
		objects, err := repository.ReachableGit(t.Context(), refs, fixture.Objects)
		if err != nil {
			t.Fatal(err)
		}
		return objects
	}
	wanted := walk(wants)
	for id := range walk(haves) {
		delete(wanted, id)
	}
	return wanted
}

func checkPreparedPack(t *testing.T, response []byte, sideband bool, expected map[string]repository.GitObject) {
	t.Helper()
	got := uploadPackObjects(t, response, sideband)
	if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(expected))) {
		t.Fatalf("wrong selected object IDs: got %v; want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(expected)))
	}
	for id, object := range expected {
		if got[id].Type != object.Type || !bytes.Equal(got[id].Data, object.Data) {
			t.Fatalf("wrong selected object %s", id)
		}
	}
}

func packCacheMetric(t testing.TB, shared *cache.Cache, name string) uint64 {
	t.Helper()
	var out bytes.Buffer
	shared.WritePrometheus(&out)
	prefix := "gitone_cache_" + name + " "
	for line := range strings.SplitSeq(out.String(), "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return parsed
		}
	}
	t.Fatalf("cache metric %s missing", name)
	return 0
}

func TestCopyPackCancellation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		sideband bool
	}{
		{name: "raw"},
		{name: "sideband", sideband: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := &cancelPackWriter{cancel: cancel}
			payload := bytes.Repeat([]byte("payload"), 20000)
			if err := copyPack(ctx, out, bytes.NewReader(payload), test.sideband); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled copy = %v", err)
			}
			if out.writes != 1 {
				t.Fatalf("continued writing after cancellation: %d writes", out.writes)
			}
		})
	}
}

type cancelPackWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelPackWriter) Write(p []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(p), nil
}

func TestUploadResponseCancellationAndClose(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	body := &closePackReader{Reader: bytes.NewReader([]byte("PACK"))}
	response := &uploadResponse{body: body, prelude: []byte(pkt("NAK\n")), sideband: true}
	var out bytes.Buffer
	if err := response.write(ctx, &out); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled response wrote %d bytes: %v", out.Len(), err)
	}
	if err := response.Close(); err != nil || !body.closed {
		t.Fatalf("response did not close its pack: %v", err)
	}
}

type closePackReader struct {
	io.Reader
	closed bool
}

func (r *closePackReader) Close() error {
	r.closed = true
	return nil
}
