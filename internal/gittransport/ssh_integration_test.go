//go:build integration

package gittransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
)

// Git's native TCP transport uses the same stateful Git wire protocol as SSH
// after its single initial service packet. This test isolates the Git adapter
// from SSH encryption/key management while exercising the native Git client.
func TestNativeGitSSHWireLifecycle(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("native git unavailable")
	}
	handler, _ := sshTestHandler(t)
	if _, err := handler.store.Create(t.Context(), "alice", repository.CreateInput{
		Name: "empty", CreatedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	endpoint := sshWireListener(t, handler)
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		// #nosec G204 -- The test supplies fixed Git arguments directly, without a shell or user input.
		command := exec.CommandContext(ctx, "git", append([]string{
			"-c", "user.name=Alice", "-c", "user.email=alice@example.test",
		}, args...)...)
		command.Dir = dir
		command.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	run(root, "ls-remote", endpoint+"/alice/demo.git")
	run(root, "clone", endpoint+"/alice/demo.git", "one")
	one := filepath.Join(root, "one")
	if got := run(one, "show", "HEAD:README.md"); got != "# demo" {
		t.Fatalf("cloned README = %q", got)
	}
	run(root, "clone", endpoint+"/alice/demo.git", "two")
	two := filepath.Join(root, "two")
	for _, content := range []string{
		strings.Repeat("a line of source code which compresses well\n", 3000),
		strings.Repeat("a line of source code which compresses well\n", 2999) + "changed\n",
	} {
		if err := os.WriteFile(filepath.Join(one, "code.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		run(one, "add", "code.txt")
		run(one, "commit", "-m", "Change code")
		run(one, "push", "origin", "main")
		run(two, "pull", "--ff-only")
		if run(one, "rev-parse", "HEAD") != run(two, "rev-parse", "HEAD") {
			t.Fatal("stateful fetch did not advance")
		}
	}
	run(one, "branch", "feature")
	run(one, "tag", "light")
	run(one, "tag", "-a", "annotated", "-m", "Release")
	run(one, "push", "--atomic", "origin", "feature", "light", "annotated")
	run(two, "fetch", "--tags")
	run(one, "push", "origin", "--delete", "feature", "light")
	run(one, "push", "origin", "main") // Up-to-date push sends no commands.
	run(two, "fetch")                  // Up-to-date fetch sends no wants.
	run(root, "clone", endpoint+"/alice/empty.git", "empty")
	empty := filepath.Join(root, "empty")
	run(empty, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(empty, "first.txt"), []byte("first push\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(empty, "add", "first.txt")
	run(empty, "commit", "-m", "First")
	run(empty, "push", "-u", "origin", "main")
	blob, err := handler.store.Blob(t.Context(), "alice", "empty", "", "first.txt")
	if err != nil || blob.Content != "first push\n" {
		t.Fatalf("pushed file unavailable: %+v %v", blob, err)
	}
}

func sshWireListener(t *testing.T, handler *Handler) string {
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
			if err := serveSSHWireTest(t.Context(), handler, connection); err != nil {
				t.Error(err)
			}
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
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

func serveSSHWireTest(ctx context.Context, handler *Handler, connection net.Conn) error {
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set test connection deadline: %w", err)
	}
	line, _, err := readPkt(connection)
	if err != nil {
		return fmt.Errorf("read test service: %w", err)
	}
	command, _, _ := strings.Cut(string(line), "\x00")
	service, path, _ := strings.Cut(command, " ")
	namespace, name, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	ctx = WithWriteAuthorization(ctx, func(context.Context) error { return nil })
	return handler.ServeSSH(ctx, SSHRequest{
		Namespace: namespace, Repository: strings.TrimSuffix(name, ".git"), Service: service, Stream: connection,
	})
}
