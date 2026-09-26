//go:build integration

package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/protocol"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

// Native LFS uses a file larger than the ordinary Git object limit. The client
// performs its own clean/smudge filters and pre-push upload hook.
func TestNativeGitLFSHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("native LFS lifecycle")
	}
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	store, err := repository.New(storage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "large", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	git, err := gittransport.New(store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	lfs, err := New(store, Options{PublicURL: "http://" + server.Listener.Addr().String(), MaxQueuedTransfers: 8})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := protocol.NewHandler(git, lfs)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatch.ServeHTTP(w, r.WithContext(gittransport.WithWriteAuthorization(r.Context(), func(context.Context) error { return nil })))
	})
	server.Start()
	defer server.Close()
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		prefix := []string{"-c", "user.name=LFS Test", "-c", "user.email=lfs@example.test",
			"-c", "filter.lfs.process=git-lfs filter-process", "-c", "filter.lfs.required=true",
			"-c", "filter.lfs.clean=git-lfs clean -- %f", "-c", "filter.lfs.smudge=git-lfs smudge -- %f"}
		// #nosec G204 -- Fixed native Git test arguments without a shell.
		cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run(root, "init", "--quiet", "-b", "main", "source")
	source := filepath.Join(root, "source")
	run(source, "lfs", "install", "--local")
	run(source, "lfs", "track", "*.bin")
	file, err := os.Create(filepath.Join(source, "asset.bin")) // #nosec G304 -- Fixed fixture path inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, copyErr := io.CopyN(io.MultiWriter(file, digest), rand.NewChaCha8([32]byte{21}), 20<<20)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	initialOID := hex.EncodeToString(digest.Sum(nil))
	run(source, "add", ".gitattributes", "asset.bin")
	run(source, "commit", "--quiet", "-m", "Add large LFS asset")
	run(source, "remote", "add", "origin", server.URL+"/alice/large.git")
	run(source, "push", "-u", "origin", "main")
	if object, err := store.LFSStat(t.Context(), "alice", "large", initialOID); err != nil || object.Size != 20<<20 {
		t.Fatalf("uploaded LFS object: %+v %v", object, err)
	}
	run(root, "clone", server.URL+"/alice/large.git", "clone")
	clone := filepath.Join(root, "clone")
	run(clone, "lfs", "fsck")
	run(clone, "fsck", "--full", "--strict")
	assertFileHash(t, filepath.Join(clone, "asset.bin"), initialOID)
	file, err = os.OpenFile(filepath.Join(source, "asset.bin"), os.O_WRONLY, 0) // #nosec G304 -- Fixed fixture path inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte("LFS asset revision"), 9<<20)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "asset.bin")
	run(source, "commit", "--quiet", "-m", "Update LFS asset")
	run(source, "push")
	run(clone, "pull", "--ff-only")
	run(clone, "lfs", "fsck")
	if run(clone, "rev-parse", "HEAD") != run(source, "rev-parse", "HEAD") {
		t.Fatal("LFS pull did not advance Git refs")
	}
	if _, err := store.CheckIntegrity(t.Context(), "alice", "large"); err != nil {
		t.Fatalf("repository and LFS integrity: %v", err)
	}
	t.Log("native LFS push, clone, update, pull, git fsck, and git lfs fsck passed for a 20 MiB object")
}

func assertFileHash(t *testing.T, path, want string) {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- Test fixture path supplied by the caller.
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("file hash %s, want %s", got, want)
	}
}
