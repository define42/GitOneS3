//go:build integration

package gittransport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

// The client and Git object creation run in separate processes. The server uses
// files for all object-store payloads, so memory measurements do not include a
// fake S3 backend retaining copies of pack bodies. Ten independent 8 MiB binary
// files exercise a wire pack and reachable history larger than the old 64 MiB
// limit. The SSH adapter deliberately keeps its read side open during responses.
func TestNativeGitLargeStreamingLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("80 MiB native Git transport regression")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("native git unavailable")
	}
	objects := &profileDiskStore{root: filepath.Join(t.TempDir(), "objects")}
	store, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "large", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	var probe, chunkedPush atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			if r.ContentLength == 4 {
				probe.Store(true)
			}
			if r.ContentLength == -1 {
				chunkedPush.Store(true)
			}
		}
		handler.ServeHTTP(w, r.WithContext(WithWriteAuthorization(r.Context(), func(context.Context) error { return nil })))
	}))
	defer server.Close()
	sshEndpoint := largeSSHWireListener(t, handler)
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		// #nosec G204 -- Fixed native Git test arguments, executed without a shell.
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "user.name=Stream Test", "-c", "user.email=stream@example.test", "-c", "pack.threads=1"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	run(root, "init", "--quiet", "-b", "main", "source")
	source := filepath.Join(root, "source")
	random := rand.NewChaCha8([32]byte{42})
	for i := range 10 {
		f, err := os.Create(filepath.Join(source, fmt.Sprintf("binary-%02d.dat", i))) // #nosec G304 -- Fixed fixture names inside t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(f, random, 8<<20)
		if err := errors.Join(copyErr, f.Close()); err != nil {
			t.Fatal(err)
		}
	}
	run(source, "add", ".")
	run(source, "commit", "--quiet", "-m", "80 MiB initial import")
	run(source, "remote", "add", "origin", server.URL+"/alice/large.git")

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peak atomic.Uint64
	peak.Store(baseline.HeapAlloc)
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > peak.Load() {
					peak.Store(stats.HeapAlloc)
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-stopped
		t.Logf("80 MiB native lifecycle: baseline heap %.1f MiB, sampled peak heap %.1f MiB; object-store reads %.1f MiB, writes %.1f MiB", float64(baseline.HeapAlloc)/(1<<20), float64(peak.Load())/(1<<20), float64(objects.readBytes.Load())/(1<<20), float64(objects.writeBytes.Load())/(1<<20))
	}()
	run(source, "push", "-u", "origin", "main")
	if !probe.Load() || !chunkedPush.Load() {
		t.Fatal("native push did not exercise the chunked request probe")
	}
	snapshot, err := store.ReadGitReferences(t.Context(), "alice", "large")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Objects != nil {
		t.Fatal("reference advertisement materialized repository objects")
	}
	if _, err := store.LoadGitObjects(t.Context(), snapshot); !errors.Is(err, repository.ErrLimit) {
		t.Fatalf("legacy materialization unexpectedly handled large repository: %v", err)
	}
	// A native client verifies every emitted object and pack checksum while cloning.
	run(root, "clone", sshEndpoint+"/alice/large.git", "clone")
	clone := filepath.Join(root, "clone")
	run(clone, "fsck", "--full", "--strict")
	if run(clone, "rev-parse", "HEAD") != run(source, "rev-parse", "HEAD") {
		t.Fatal("clone ref differs from pushed ref")
	}
	// HTTP stages a complete response on disk before sending headers; exercise
	// that full-clone path independently of SSH's direct pack output.
	run(root, "clone", server.URL+"/alice/large.git", "http-clone")
	httpClone := filepath.Join(root, "http-clone")
	run(httpClone, "fsck", "--full", "--strict")
	// Change a few bytes of an 8 MiB blob, producing a native thin REF delta.
	file, err := os.OpenFile(filepath.Join(clone, "binary-00.dat"), os.O_WRONLY, 0) // #nosec G304 -- Fixed fixture name inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte("streaming delta revision"), 4<<20)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	run(clone, "add", "binary-00.dat")
	run(clone, "commit", "--quiet", "-m", "Small edit of large binary")
	run(clone, "push", "origin", "main")
	run(httpClone, "fetch", "origin")
	run(httpClone, "merge", "--ff-only", "origin/main")
	run(httpClone, "fsck", "--full", "--strict")
	if run(httpClone, "rev-parse", "HEAD") != run(clone, "rev-parse", "HEAD") {
		t.Fatal("incremental fetch did not advance")
	}
	// Preserve atomic multi-ref updates and deletion on the large repository.
	run(clone, "branch", "feature")
	run(clone, "tag", "large-import")
	run(clone, "push", "--atomic", "origin", "feature", "large-import")
	run(httpClone, "fetch", "--tags")
	run(clone, "push", "origin", "--delete", "feature", "large-import")
	report, err := store.CheckIntegrity(t.Context(), "alice", "large")
	if err != nil || report.Bytes < 88<<20 || report.Objects < 15 || report.Packs < 2 {
		t.Fatalf("streamed repository integrity: %+v %v", report, err)
	}
	snapshot, err = store.ReadGitReferences(t.Context(), "alice", "large")
	if err != nil || snapshot.Objects != nil {
		t.Fatalf("final refs materialized bodies: %v", err)
	}
}

func largeSSHWireListener(t *testing.T, handler *Handler) string {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			connection, err := listener.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			err = func() (resultErr error) {
				defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
				if err := connection.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
					return err
				}
				r := bufio.NewReader(connection)
				line, _, err := readPkt(r)
				if err != nil {
					return err
				}
				command, _, _ := strings.Cut(string(line), "\x00")
				service, path, _ := strings.Cut(command, " ")
				namespace, name, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
				ctx := WithWriteAuthorization(t.Context(), func(context.Context) error { return nil })
				return handler.ServeSSH(ctx, SSHRequest{Namespace: namespace, Repository: strings.TrimSuffix(name, ".git"), Service: service, Stream: bufferedTestConnection{Conn: connection, reader: r}})
			}()
			if err != nil {
				t.Error(err)
			}
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		<-finished
	})
	return "git://" + listener.Addr().String()
}

type bufferedTestConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedTestConnection) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Complete the disk fixture's ObjectStore interface for pack range reads and
// maintenance locks. This also keeps the existing optional memory harness useful.
func (s *profileDiskStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if offset < 0 || length <= 0 {
		return nil, storage.ObjectInfo{}, storage.ErrInvalidRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(key)
	info, err := profileInfo(key, path)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if offset >= info.Size {
		return nil, storage.ObjectInfo{}, storage.ErrInvalidRange
	}
	file, err := os.Open(path) // #nosec G304 -- Object paths come from repository code inside the private test fixture root.
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	s.gets.Add(1)
	rangeReader := &profileRangeCloser{Reader: io.NewSectionReader(file, offset, min(length, info.Size-offset)), Closer: file}
	return &profileReadCloser{ReadCloser: rangeReader, count: &s.readBytes}, info, nil
}

type profileRangeCloser struct {
	io.Reader
	io.Closer
}

func (s *profileDiskStore) Delete(ctx context.Context, key string, version storage.Version) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(key)
	info, err := profileInfo(key, path)
	if err != nil {
		return err
	}
	if version != "" && version != info.Version {
		return storage.ErrPreconditionFailed
	}
	return os.Remove(path)
}

func (s *profileDiskStore) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := map[string]storage.ObjectInfo{}
	for _, root := range []string{s.root, s.overlay} {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			key, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			key = filepath.ToSlash(key)
			if !strings.HasPrefix(key, prefix) {
				return nil
			}
			info, err := profileInfo(key, path)
			if err != nil {
				return err
			}
			entries[key] = info
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	result := make([]storage.ObjectInfo, 0, len(entries))
	for _, info := range entries {
		result = append(result, info)
	}
	slices.SortFunc(result, func(a, b storage.ObjectInfo) int { return strings.Compare(a.Key, b.Key) })
	return result, nil
}

func (s *profileDiskStore) ListPage(ctx context.Context, prefix, after string, limit int) (storage.ObjectPage, error) {
	if err := storage.ValidateListPage(prefix, after, limit); err != nil {
		return storage.ObjectPage{}, err
	}
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return storage.ObjectPage{}, err
	}
	page := storage.ObjectPage{}
	for _, object := range objects {
		if object.Key <= after {
			continue
		}
		if len(page.Objects) == limit {
			page.NextAfter = page.Objects[len(page.Objects)-1].Key
			break
		}
		page.Objects = append(page.Objects, object)
	}
	return page, nil
}
