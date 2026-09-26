//go:build integration

package gittransport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

// TestHandlerPeakMemory is intentionally opt-in and serial. Example:
//
//	GITONE_PROFILE_MEMORY=1 go test -tags=integration ./internal/gittransport \
//	  -run '^TestHandlerPeakMemory$' -count=1 -v -timeout=15m
//
// Repeat with GITONE_PROFILE_CLIENTS=1, 2 and 4 to exercise the same production
// admission limit. GITONE_PROFILE_SCENARIOS accepts a comma-separated subset of
// clone,incremental,slow-clone,push,delta-push,mixed. GITONE_PROFILE_MIB defaults
// to 63; a smaller value (1..63) supports fast harness/race smoke checks only.
//
// Seed creation runs in another process. Each measured worker starts fresh,
// uses one real production Handler, an on-disk ObjectStore, and loopback HTTP
// clients with bounded buffers (no MemoryStore or ResponseRecorder). Heap peak
// is sampled every 2 ms and may miss shorter peaks; Linux VmHWM is the kernel's
// process RSS high-water mark. Both include the tiny HTTP clients/sampler, but
// neither includes the seed process, fixture buffers or filesystem page cache.
// This is a capacity experiment, not an HTTP latency benchmark or an S3 test.
func TestHandlerPeakMemory(t *testing.T) {
	if os.Getenv("GITONE_PROFILE_MEMORY") != "1" {
		t.Skip("set GITONE_PROFILE_MEMORY=1 to run isolated peak-memory measurements")
	}
	if runtime.GOOS != "linux" {
		t.Skip("RSS high-water measurement requires Linux /proc")
	}
	if role := os.Getenv("GITONE_PROFILE_ROLE"); role != "" {
		if role == "seed" {
			seedMemoryFixture(t, os.Getenv("GITONE_PROFILE_FIXTURE"))
		} else {
			measureMemoryWorker(t)
		}
		return
	}
	fixture := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, role, scenario string) {
		t.Helper()
		// #nosec G204 -- Re-exec this test binary with fixed test arguments; no shell.
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestHandlerPeakMemory$", "-test.v", "-test.timeout=10m")
		cmd.Env = append(os.Environ(), "GITONE_PROFILE_ROLE="+role, "GITONE_PROFILE_FIXTURE="+fixture, "GITONE_PROFILE_SCENARIO="+scenario)
		output, err := cmd.CombinedOutput()
		t.Logf("%s", output)
		if err != nil {
			t.Fatalf("%s/%s: %v", role, scenario, err)
		}
	}
	run(t, "seed", "")
	for _, scenario := range []string{"clone", "incremental", "slow-clone", "push", "delta-push", "mixed"} {
		if only := os.Getenv("GITONE_PROFILE_SCENARIOS"); only != "" && !strings.Contains(","+only+",", ","+scenario+",") {
			continue
		}
		t.Run(scenario, func(t *testing.T) { run(t, "worker", scenario) })
	}
}

type memoryFixture struct {
	Old, Head string
	Bytes     int64
}

func seedMemoryFixture(t *testing.T, root string) {
	t.Helper()
	objects := &profileDiskStore{root: filepath.Join(root, "objects")}
	store, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	megabytes := 63
	if value := os.Getenv("GITONE_PROFILE_MIB"); value != "" {
		megabytes, err = strconv.Atoi(value)
		if err != nil || megabytes < 1 || megabytes > 63 {
			t.Fatal("GITONE_PROFILE_MIB must be 1..63")
		}
	}
	snap, old, head := benchmarkSnapshot(megabytes)
	for index := range 4 {
		name := fmt.Sprintf("repo-%d", index)
		if _, err := store.Create(t.Context(), "perf", repository.CreateInput{Name: name, CreatedBy: "perf"}); err != nil {
			t.Fatal(err)
		}
		base, err := store.ReadGit(t.Context(), "perf", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PublishGit(t.Context(), base, []repository.RefUpdate{{Name: "refs/heads/main", New: head}}, snap.Objects, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for _, object := range snap.Objects {
		total += int64(len(object.Data))
	}
	metadata, err := json.Marshal(memoryFixture{Old: old, Head: head, Bytes: total})
	if err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "fixture.json"), metadata)
	// Force-update to a new unrelated graph of the same near-limit size. This
	// preserves the old snapshot in memory while decoding a complete new one.
	incoming := make(map[string]repository.GitObject)
	var tree []byte
	var delta packFixture
	for index := range megabytes {
		entry := fmt.Sprintf("100644 file-%03d\x00", index)
		var baseID string
		for _, object := range snap.Objects {
			if object.Type == "tree" {
				position := bytes.Index(object.Data, []byte(entry)) + len(entry)
				baseID = hex.EncodeToString(object.Data[position : position+20])
				break
			}
		}
		base := snap.Objects[baseID]
		data := bytes.Clone(base.Data)
		data[0] ^= 0xff
		object := repository.GitObject{Type: "blob", Data: data}
		id := repository.GitObjectID(object)
		incoming[id] = object
		hash, _ := hex.DecodeString(id)
		tree = append(append(tree, entry...), hash...)
		instructions := binary.AppendUvarint(nil, uint64(len(data)))
		instructions = binary.AppendUvarint(instructions, uint64(len(data)))
		// Insert one changed byte; copy bytes 1..1048575 from the old blob.
		instructions = append(instructions, 1, data[0], 0xf1, 1, 0xff, 0xff, 0x0f)
		baseHash, _ := hex.DecodeString(baseID)
		delta.add(t, 7, baseHash, instructions)
	}
	treeObject := repository.GitObject{Type: "tree", Data: tree}
	treeID := repository.GitObjectID(treeObject)
	incoming[treeID] = treeObject
	commit := repository.GitObject{Type: "commit", Data: []byte("tree " + treeID + "\nauthor Perf <perf@example.test> 2 +0000\ncommitter Perf <perf@example.test> 2 +0000\n\nreplacement\n")}
	newHead := repository.GitObjectID(commit)
	incoming[newHead] = commit
	delta.add(t, 2, nil, tree)
	delta.add(t, 1, nil, commit.Data)
	commands := []byte(pkt(head+" "+newHead+" refs/heads/main\x00report-status\n") + "0000")
	writeProfileFile(t, filepath.Join(root, "delta-push.bin"), append(bytes.Clone(commands), delta.finish()...))
	pack, err := encodePack(t.Context(), incoming)
	if err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, filepath.Join(root, "push.bin"), append(commands, pack...))
}

func writeProfileFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func measureMemoryWorker(t *testing.T) {
	t.Helper()
	root := os.Getenv("GITONE_PROFILE_FIXTURE")
	scenario := os.Getenv("GITONE_PROFILE_SCENARIO")
	data, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture memoryFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	concurrency := 1
	if value := os.Getenv("GITONE_PROFILE_CLIENTS"); value != "" {
		concurrency, err = strconv.Atoi(value)
		if err != nil || concurrency < 1 || concurrency > 4 {
			t.Fatal("GITONE_PROFILE_CLIENTS must be 1..4")
		}
	}
	objects := &profileDiskStore{root: filepath.Join(root, "objects"), overlay: t.TempDir()}
	store, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(store, Options{
		MaxConcurrentOperations: concurrency,
		MaxQueuedOperations:     4,
		QueueTimeout:            5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(WithWriteAuthorization(r.Context(), func(context.Context) error { return nil })))
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Minute
	runtime.GC()
	var initial runtime.MemStats
	runtime.ReadMemStats(&initial)
	peakHeap, peakSys := initial.HeapAlloc, initial.HeapSys
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			peakHeap = max(peakHeap, stats.HeapAlloc)
			peakSys = max(peakSys, stats.HeapSys)
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	start := time.Now()
	var responseBytes atomic.Int64
	errors := make(chan error, concurrency)
	var workers sync.WaitGroup
	startWorkers := make(chan struct{})
	for index := range concurrency {
		workers.Go(func() {
			<-startWorkers
			if scenario == "mixed" {
				operations := []string{"slow-clone", "delta-push", "incremental", "push"}
				for operation := index; operation < len(operations); operation += concurrency {
					count, err := profileHTTPRequest(t.Context(), client, server.URL, root, operations[operation], operation, fixture)
					responseBytes.Add(count)
					if err != nil {
						errors <- err
						return
					}
				}
				errors <- nil
				return
			}
			count, err := profileHTTPRequest(t.Context(), client, server.URL, root, scenario, index, fixture)
			responseBytes.Add(count)
			errors <- err
		})
	}
	close(startWorkers)
	workers.Wait()
	close(stop)
	<-sampled
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
	var final runtime.MemStats
	runtime.ReadMemStats(&final)
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	var rssKiB uint64
	for line := range strings.SplitSeq(string(status), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			rssKiB, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	result := map[string]any{
		"scenario": scenario, "clients": concurrency, "active_limit": concurrency, "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"fixture_bytes": fixture.Bytes, "peak_rss_bytes": rssKiB * 1024, "initial_heap_bytes": initial.HeapAlloc,
		"sampled_peak_heap_bytes": peakHeap, "sampled_peak_heap_sys_bytes": peakSys,
		"allocated_bytes": final.TotalAlloc - initial.TotalAlloc, "gc_cycles": final.NumGC - initial.NumGC,
		"elapsed_seconds": time.Since(start).Seconds(), "response_bytes": responseBytes.Load(),
		"store_read_bytes": objects.readBytes.Load(), "store_write_bytes": objects.writeBytes.Load(), "store_gets": objects.gets.Load(),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GITONE_MEMORY %s", encoded)
}

func profileHTTPRequest(ctx context.Context, client *http.Client, baseURL, root, operation string, index int, fixture memoryFixture) (int64, error) {
	service := upload
	body := io.Reader(strings.NewReader(pkt("want "+fixture.Head+" side-band-64k\n") + "0000" + pkt("done\n")))
	if operation == "incremental" {
		body = strings.NewReader(pkt("want "+fixture.Head+" side-band-64k\n") + "0000" + pkt("have "+fixture.Old+"\n") + pkt("done\n"))
	}
	if operation == "push" || operation == "delta-push" {
		service = receive
		file, err := os.Open(filepath.Join(root, operation+".bin"))
		if err != nil {
			return 0, err
		}
		defer file.Close()
		body = file
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/perf/repo-%d.git/%s", baseURL, index, service), body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/x-"+service+"-request")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: HTTP %d", operation, response.StatusCode)
	}
	if service == receive {
		data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return 0, err
		}
		if !bytes.Contains(data, []byte("unpack ok\n")) || !bytes.Contains(data, []byte("ok refs/heads/main\n")) || bytes.Contains(data, []byte("ng ")) {
			return int64(len(data)), fmt.Errorf("push failed: %s", data)
		}
		return int64(len(data)), nil
	}
	if operation == "slow-clone" {
		buffer := make([]byte, 64<<10)
		var total int64
		for {
			n, err := response.Body.Read(buffer)
			total += int64(n)
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			if err != nil {
				return total, err
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	return io.Copy(io.Discard, response.Body)
}

// profileDiskStore implements just the operations exercised by repository
// creation/ReadGit/PublishGit. Unexpected ObjectStore operations fail loudly via
// the nil embedded interface. Conditional writes and readers are mutex-protected;
// content lives in files, never a retained map of byte slices.
type profileDiskStore struct {
	storage.ObjectStore
	root, overlay string
	mu            sync.Mutex
	readBytes     atomic.Int64
	writeBytes    atomic.Int64
	gets          atomic.Int64
}

func (s *profileDiskStore) path(key string) string {
	if s.overlay != "" {
		path := filepath.Join(s.overlay, key)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(s.root, key)
}

func profileInfo(key, path string) (storage.ObjectInfo, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{Key: key, Size: info.Size(), LastModified: info.ModTime(), Version: storage.Version(fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size()))}, nil
}

func (s *profileDiskStore) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return profileInfo(key, s.path(key))
}

func (s *profileDiskStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(key)
	info, err := profileInfo(key, path)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	s.gets.Add(1)
	return &profileReadCloser{ReadCloser: file, count: &s.readBytes}, info, nil
}

type profileReadCloser struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *profileReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	r.count.Add(int64(n))
	return n, err
}

func (s *profileDiskStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, err := profileInfo(key, s.path(key))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return storage.ObjectInfo{}, err
	}
	if options.IfNoneMatch && err == nil {
		return storage.ObjectInfo{}, storage.ErrAlreadyExists
	}
	if options.IfMatch != "" && (err != nil || prior.Version != options.IfMatch) {
		return storage.ObjectInfo{}, storage.ErrPreconditionFailed
	}
	root := s.root
	if s.overlay != "" {
		root = s.overlay
	}
	path := filepath.Join(root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return storage.ObjectInfo{}, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "profile-object-")
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || n != size {
		return storage.ObjectInfo{}, fmt.Errorf("profile object write size %d/%d: %w", n, size, errors.Join(copyErr, closeErr))
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return storage.ObjectInfo{}, err
	}
	s.writeBytes.Add(n)
	return profileInfo(key, path)
}
