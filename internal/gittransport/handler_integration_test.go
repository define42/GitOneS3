//go:build integration

package gittransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestNativeGitLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("native git unavailable")
	}
	store, err := repository.New(storage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo", "empty"} {
		if _, err := store.Create(context.Background(), "alice", repository.CreateInput{Name: name, CreatedBy: "alice", AuthorName: "Alice", AuthorEmail: "alice@example.test", InitializeReadme: name == "demo"}); err != nil {
			t.Fatal(err)
		}
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
	run := func(dir string, args ...string) string {
		t.Helper()
		// #nosec G204 -- The test supplies fixed Git arguments directly, without a shell or user input.
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-c", "user.name=Alice", "-c", "user.email=alice@example.test",
		}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	run(root, "clone", server.URL+"/alice/demo.git", "one")
	one := filepath.Join(root, "one")
	if got := run(one, "show", "HEAD:README.md"); got != "# demo" {
		t.Fatalf("README = %q", got)
	}
	content := strings.Repeat("a line of source code which compresses well\n", 3000)
	if err := os.WriteFile(filepath.Join(one, "code.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	run(one, "add", "code.txt")
	run(one, "commit", "-m", "Add code")
	run(one, "push", "origin", "main")
	run(root, "clone", server.URL+"/alice/demo.git", "two")
	content = strings.Replace(content, "source", "changed", 1)
	if err := os.WriteFile(filepath.Join(one, "code.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	run(one, "add", "code.txt")
	run(one, "commit", "-m", "Change code")
	run(one, "push", "origin", "main")
	run(filepath.Join(root, "two"), "pull", "--ff-only")
	if run(one, "rev-parse", "HEAD") != run(filepath.Join(root, "two"), "rev-parse", "HEAD") {
		t.Fatal("pull did not advance")
	}
	run(one, "branch", "feature")
	run(one, "tag", "light")
	run(one, "tag", "-a", "annotated", "-m", "Release")
	run(one, "push", "--atomic", "origin", "feature", "light", "annotated")
	run(filepath.Join(root, "two"), "fetch", "--tags")
	run(one, "push", "origin", "--delete", "feature", "light")
	run(root, "clone", server.URL+"/alice/empty.git", "empty")
	empty := filepath.Join(root, "empty")
	run(empty, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(empty, "first.txt"), []byte("first push\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(empty, "add", "first.txt")
	run(empty, "commit", "-m", "First")
	run(empty, "push", "-u", "origin", "main")
	blob, err := store.Blob(context.Background(), "alice", "empty", "", "first.txt")
	if err != nil || blob.Content != "first push\n" {
		t.Fatalf("pushed file unavailable: %+v %v", blob, err)
	}
	run(root, "clone", server.URL+"/alice/empty.git", "empty-copy")
}
