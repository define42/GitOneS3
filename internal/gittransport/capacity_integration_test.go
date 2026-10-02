//go:build integration

package gittransport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

// TestNativeGitCapacity deliberately retains the production 90-second transfer
// deadlines. Run on deployment-sized CPU, memory and disk, without -race:
//
//	GITONE_QUALIFY_CAPACITY=bytes go test -tags=integration ./internal/gittransport \
//	  -run '^TestNativeGitCapacity$' -count=1 -v -timeout=20m
//
// Modes bytes, objects and both cover 1023 MiB and exactly 100,000 objects. The
// streaming native fixture avoids the legacy helper's 63 MiB materialization
// ceiling. The S3 qualification harness can reuse runNativeGitCapacity.
func TestNativeGitCapacity(t *testing.T) {
	mode := os.Getenv("GITONE_QUALIFY_CAPACITY")
	if mode == "" || testing.Short() {
		t.Skip("set GITONE_QUALIFY_CAPACITY=bytes, objects or both for supported-limit qualification")
	}
	if mode != "bytes" && mode != "objects" && mode != "both" {
		t.Fatal("GITONE_QUALIFY_CAPACITY must be bytes, objects or both")
	}
	for _, current := range []string{"bytes", "objects"} {
		if mode != "both" && mode != current {
			continue
		}
		t.Run(current, func(t *testing.T) {
			objects := &profileDiskStore{root: filepath.Join(t.TempDir(), "objects")}
			runNativeGitCapacity(t, objects, current)
		})
	}
}

func TestNativeGitCapacityFixture(t *testing.T) {
	for _, mode := range []string{"bytes", "objects"} {
		t.Run(mode, func(t *testing.T) {
			objects := &profileDiskStore{root: filepath.Join(t.TempDir(), "objects")}
			plan := capacityPlan{blobs: 7, bytesPerBlob: 4096, blobsPerDirectory: 3}
			runNativeGitCapacityPlan(t, objects, mode, plan)
		})
	}
}

type capacityPlan struct {
	blobs, bytesPerBlob, finalBlobBytes, blobsPerDirectory int
}

func (p capacityPlan) objectCount() int {
	return p.blobs + (p.blobs+p.blobsPerDirectory-1)/p.blobsPerDirectory + 2 // directories, root tree, commit
}

func runNativeGitCapacity(t *testing.T, objects storage.ObjectStore, mode string) {
	t.Helper()
	var plan capacityPlan
	switch mode {
	case "bytes":
		plan = capacityPlan{blobs: 64, bytesPerBlob: 16 << 20, finalBlobBytes: 15 << 20, blobsPerDirectory: 1000}
	case "objects":
		plan = capacityPlan{blobs: 99_898, blobsPerDirectory: 1000}
	default:
		t.Fatalf("unknown capacity mode %q", mode)
	}
	runNativeGitCapacityPlan(t, objects, mode, plan)
}

func runNativeGitCapacityPlan(t *testing.T, objects storage.ObjectStore, mode string, plan capacityPlan) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("native git unavailable")
	}
	measured := &capacityObjectStore{ObjectStore: objects}
	store, err := repository.New(measured)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "capacity", repository.CreateInput{Name: mode, CreatedBy: "capacity"}); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(WithWriteAuthorization(r.Context(), func(context.Context) error { return nil })))
	}))
	defer server.Close()
	root := t.TempDir()
	nativeCapacityGit(t, root, "init", "--quiet", "--bare", "source.git")
	source := filepath.Join(root, "source.git")
	seedNativeCapacityGit(t, source, mode, plan)
	head := nativeCapacityGit(t, source, "rev-parse", "refs/heads/main")
	url := server.URL + "/capacity/" + mode + ".git"
	measure := func(operation string, run func()) {
		t.Helper()
		read, written, gets := measured.readBytes.Load(), measured.writeBytes.Load(), measured.gets.Load()
		start := time.Now()
		defer func() {
			result, err := json.Marshal(map[string]any{
				"mode": mode, "operation": operation, "objects": plan.objectCount(),
				"go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "elapsed_seconds": time.Since(start).Seconds(),
				"store_read_bytes": measured.readBytes.Load() - read, "store_write_bytes": measured.writeBytes.Load() - written,
				"store_gets": measured.gets.Load() - gets,
			})
			if err != nil {
				t.Error(err)
				return
			}
			t.Logf("GITONE_CAPACITY %s", result)
		}()
		run()
	}
	measure("push", func() { nativeCapacityGit(t, source, "push", url, "refs/heads/main") })
	measure("clone", func() { nativeCapacityGit(t, root, "clone", "--no-checkout", "--branch", "main", url, "clone") })
	clone := filepath.Join(root, "clone")
	measure("native-fsck", func() { nativeCapacityGit(t, clone, "fsck", "--full", "--strict") })
	if got := nativeCapacityGit(t, clone, "rev-parse", "HEAD"); got != head {
		t.Fatalf("cloned head=%s, want %s", got, head)
	}
	listed := nativeCapacityGit(t, clone, "rev-list", "--objects", "--all", "--no-object-names")
	count := strings.Count(listed, "\n") + 1
	if count != plan.objectCount() {
		t.Fatalf("cloned objects=%d, want %d", count, plan.objectCount())
	}
	measure("integrity", func() {
		report, err := store.CheckIntegrity(t.Context(), "capacity", mode)
		if err != nil || report.Objects != plan.objectCount() {
			t.Fatalf("capacity integrity: %+v: %v", report, err)
		}
		if mode == "bytes" && plan.blobs == 64 && (report.Bytes < 1023<<20 || report.Bytes > repository.MaxGitBytes) {
			t.Fatalf("fixture did not approach the decoded byte limit: %d", report.Bytes)
		}
	})
}

func capacityGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	// #nosec G204 -- Fixed native Git qualification commands, no shell. URLs come from httptest and paths from t.TempDir.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.compression=0", "-c", "pack.threads=1", "-c", "pack.window=0"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd
}

func nativeCapacityGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	output, err := capacityGitCommand(ctx, dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func seedNativeCapacityGit(t *testing.T, dir, mode string, plan capacityPlan) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	// fast-import has its own previous-blob delta search; pack.window=0 does
	// not disable it. These independent fixture blobs need no delta search,
	// and streaming large blobs keeps seed preparation fast and bounded.
	cmd := capacityGitCommand(ctx, dir, "fast-import", "--quiet", "--done", "--depth=0", "--big-file-threshold=1m")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	writeErr := writeCapacityImport(input, mode, plan)
	if err := errors.Join(writeErr, input.Close(), cmd.Wait()); err != nil {
		t.Fatalf("native capacity fixture: %s: %v", stderr.String(), err)
	}
}

func writeCapacityImport(output io.Writer, mode string, plan capacityPlan) error {
	w := bufio.NewWriterSize(output, 64<<10)
	random := rand.NewChaCha8([32]byte{99})
	for index := range plan.blobs {
		data := fmt.Sprintf("capacity object %06d\n", index)
		size := len(data)
		if mode == "bytes" {
			size = plan.bytesPerBlob
			if index == plan.blobs-1 && plan.finalBlobBytes > 0 {
				size = plan.finalBlobBytes
			}
		}
		if _, err := fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", index+1, size); err != nil {
			return err
		}
		if mode == "bytes" {
			if _, err := io.CopyN(w, random, int64(size)); err != nil {
				return err
			}
		} else if _, err := io.WriteString(w, data); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	message := "capacity fixture\n"
	if _, err := fmt.Fprintf(w, "commit refs/heads/main\ncommitter Capacity <capacity@example.test> 1 +0000\ndata %d\n%s", len(message), message); err != nil {
		return err
	}
	for index := range plan.blobs {
		if _, err := fmt.Fprintf(w, "M 100644 :%d dir-%03d/file-%06d\n", index+1, index/plan.blobsPerDirectory, index); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\ndone\n"); err != nil {
		return err
	}
	return w.Flush()
}

type capacityObjectStore struct {
	storage.ObjectStore
	readBytes, writeBytes, gets atomic.Int64
}

func (s *capacityObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, key)
	if err != nil {
		return nil, info, err
	}
	s.gets.Add(1)
	return &profileReadCloser{ReadCloser: body, count: &s.readBytes}, info, nil
}

func (s *capacityObjectStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.GetRange(ctx, key, offset, length)
	if err != nil {
		return nil, info, err
	}
	s.gets.Add(1)
	return &profileReadCloser{ReadCloser: body, count: &s.readBytes}, info, nil
}

func (s *capacityObjectStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.ObjectInfo, error) {
	info, err := s.ObjectStore.Put(ctx, key, body, size, options)
	if err == nil {
		s.writeBytes.Add(size)
	}
	return info, err
}
