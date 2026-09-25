//go:build integration

package sshserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

type nativeSSHRegistry struct {
	mu                   sync.Mutex
	username, group      string
	key                  []byte
	readOnly, revoked    bool
	revokeAfterAdmission bool
	verified, authorized map[shard.ShardID]int
}

type nativeSSHAuthority struct {
	registry *nativeSSHRegistry
	router   *shard.Router
	local    shard.ShardID
}

func (a nativeSSHAuthority) VerifySSHKey(_ context.Context, username string, key []byte) (auth.SSHPrincipal, error) {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	owner, err := a.router.Owner(username)
	if err != nil || owner != a.local || username != a.registry.username ||
		a.registry.revoked || !bytes.Equal(key, a.registry.key) {
		return auth.SSHPrincipal{}, repository.ErrForbidden
	}
	a.registry.verified[a.local]++
	return auth.SSHPrincipal{
		Username: username, Identity: auth.Identity{Issuer: "https://issuer.example", Subject: "subject"},
	}, nil
}

func (a nativeSSHAuthority) AuthorizeSSH(
	_ context.Context,
	principal auth.SSHPrincipal,
	namespace string,
	write bool,
) error {
	a.registry.mu.Lock()
	defer a.registry.mu.Unlock()
	owner, err := a.router.Owner(namespace)
	if err != nil || owner != a.local || principal.Username != a.registry.username ||
		principal.Identity.Subject != "subject" || principal.Identity.Issuer != "https://issuer.example" {
		return repository.ErrForbidden
	}
	if namespace != a.registry.username && namespace != a.registry.group {
		return repository.ErrForbidden
	}
	if write && a.registry.readOnly {
		return repository.ErrForbidden
	}
	a.registry.authorized[a.local]++
	if write && a.registry.revokeAfterAdmission {
		// Revoke immediately after the initial ACL admission. A push already
		// receiving its pack must re-check the user shard before publication.
		a.registry.revoked = true
	}
	return nil
}

type nativeSSHFixture struct {
	registry       *nativeSSHRegistry
	addresses      []string
	stores         []*repository.Store
	user, host     ssh.Signer
	forward        ssh.Signer
	key, knownHost string
	stop           func()
}

func newNativeSSHFixture(t *testing.T) *nativeSSHFixture {
	t.Helper()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(4, parser)
	if err != nil {
		t.Fatal(err)
	}
	userKey, userPrivate := nativeSSHSigner(t)
	hostKey, _ := nativeSSHSigner(t)
	forwardKey, _ := nativeSSHSigner(t)
	registry := &nativeSSHRegistry{
		username: nativeSSHNamespace(t, router, "alice", 0),
		group:    nativeSSHNamespace(t, router, "team", 2), key: userKey.PublicKey().Marshal(),
		verified: map[shard.ShardID]int{}, authorized: map[shard.ShardID]int{},
	}
	fixture := &nativeSSHFixture{
		registry: registry, addresses: make([]string, 4), stores: make([]*repository.Store, 4),
		user: userKey, host: hostKey, forward: forwardKey,
	}
	listeners := make([]net.Listener, 4)
	for i := range listeners {
		var config net.ListenConfig
		listeners[i], err = config.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		fixture.addresses[i] = listeners[i].Addr().String()
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	fixture.stop = func() { cancel(); workers.Wait() }
	t.Cleanup(func() {
		cancel()
		for _, listener := range listeners {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		}
		workers.Wait()
	})
	for i, listener := range listeners {
		store, err := repository.New(storage.NewMemoryStore())
		if err != nil {
			t.Fatal(err)
		}
		fixture.stores[i] = store
		git, err := gittransport.New(store)
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G115 -- i is an index into the four-element shard listener slice.
		local := shard.ShardID(i)
		server, err := New(Options{
			Address: fixture.addresses[i], Router: router, LocalShard: local,
			PeerAddress: func(owner shard.ShardID) (string, error) {
				if owner >= 4 {
					return "", errors.New("invalid shard")
				}
				return fixture.addresses[owner], nil
			},
			HostKey: hostKey, ForwardKey: forwardKey, Git: git,
			Authority: nativeSSHAuthority{registry: registry, router: router, local: local},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			if err := server.Serve(ctx, listener); err != nil {
				t.Error(err)
			}
		})
	}
	for _, namespace := range []string{registry.username, registry.group} {
		owner, err := router.Owner(namespace)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.stores[owner].Create(t.Context(), namespace, repository.CreateInput{
			Name: "demo", CreatedBy: registry.username, AuthorName: "Alice",
			AuthorEmail: "alice@example.test", InitializeReadme: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys := t.TempDir()
	fixture.key, fixture.knownHost = filepath.Join(keys, "identity"), filepath.Join(keys, "known_hosts")
	if err := os.WriteFile(fixture.key, userPrivate, 0600); err != nil {
		t.Fatal(err)
	}
	var knownHosts strings.Builder
	for _, address := range fixture.addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		knownHosts.WriteString("[" + host + "]:" + port + " " + string(ssh.MarshalAuthorizedKey(hostKey.PublicKey())))
	}
	if err := os.WriteFile(fixture.knownHost, []byte(knownHosts.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func nativeSSHSigner(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ssh.MarshalPrivateKey(private, "GitOne test")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(encoded)
}

func nativeSSHNamespace(t *testing.T, router *shard.Router, prefix string, target shard.ShardID) string {
	t.Helper()
	for i := range 1000 {
		name := fmt.Sprintf("%s%d", prefix, i)
		owner, err := router.Owner(name)
		if err != nil {
			t.Fatal(err)
		}
		if owner == target {
			return name
		}
	}
	t.Fatal("unable to find a namespace on the target shard")
	return ""
}

func (f *nativeSSHFixture) git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	// #nosec G204 -- Native Git receives only test-owned arguments and generated fixture paths.
	command := exec.CommandContext(ctx, "git", append([]string{
		"-c", "user.name=Alice", "-c", "user.email=alice@example.test", "-c", "protocol.version=0",
	}, args...)...)
	command.Dir = dir
	sshCommand := fmt.Sprintf(
		"ssh -F /dev/null -i %q -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%q -o ConnectTimeout=5 -o BatchMode=yes",
		f.key, f.knownHost,
	)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+sshCommand,
	)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func (f *nativeSSHFixture) gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := f.git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return output
}

func (f *nativeSSHFixture) url(entry int, namespace string) string {
	return "ssh://" + f.registry.username + "@" + f.addresses[entry] + "/" + namespace + "/demo.git"
}

func TestNativeSSHShardLifecycle(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"git", "ssh"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("native %s unavailable", command)
		}
	}
	fixture := newNativeSSHFixture(t)
	root := t.TempDir()
	// Entry shard 1, user authority shard 0 and repository shard 2 are distinct.
	groupURL := fixture.url(1, fixture.registry.group)
	fixture.gitRun(t, root, "clone", groupURL, "one")
	one := filepath.Join(root, "one")
	for range 40 {
		fixture.gitRun(t, one, "commit", "--allow-empty", "-m", "Build negotiation history")
	}
	fixture.gitRun(t, one, "push", "origin", "main")
	fixture.gitRun(t, root, "clone", fixture.url(3, fixture.registry.group), "two")
	two := filepath.Join(root, "two")
	fixture.gitRun(t, one, "commit", "--allow-empty", "-m", "Fetch across negotiation rounds")
	fixture.gitRun(t, one, "push", "origin", "main")
	fixture.gitRun(t, two, "pull", "--ff-only")
	if fixture.gitRun(t, one, "rev-parse", "HEAD") != fixture.gitRun(t, two, "rev-parse", "HEAD") {
		t.Fatal("cross-shard fetch did not advance")
	}
	fixture.gitRun(t, one, "branch", "feature")
	fixture.gitRun(t, one, "tag", "-a", "release", "-m", "Release")
	fixture.gitRun(t, one, "push", "--atomic", "origin", "feature", "release")
	fixture.gitRun(t, one, "push", "origin", "--delete", "feature")
	for entry := range 4 {
		fixture.gitRun(t, root, "ls-remote", fixture.url(entry, fixture.registry.group))
		fixture.gitRun(t, root, "ls-remote", fixture.url(entry, fixture.registry.username))
	}
	fixture.registry.mu.Lock()
	fixture.registry.readOnly = true
	fixture.registry.mu.Unlock()
	fixture.gitRun(t, one, "commit", "--allow-empty", "-m", "Read-only users cannot push")
	if output, err := fixture.git(t, one, "push", "origin", "main"); err == nil {
		t.Fatalf("read-only group push succeeded: %s", output)
	}
	fixture.gitRun(t, root, "ls-remote", groupURL)
	fixture.registry.mu.Lock()
	fixture.registry.readOnly = false
	fixture.registry.mu.Unlock()
	fixture.gitRun(t, one, "push", "origin", "main")
	before, err := fixture.stores[2].ReadGit(t.Context(), fixture.registry.group, "demo")
	if err != nil {
		t.Fatal(err)
	}
	fixture.gitRun(t, one, "commit", "--allow-empty", "-m", "Revoke key after admission")
	fixture.registry.mu.Lock()
	fixture.registry.revokeAfterAdmission = true
	fixture.registry.mu.Unlock()
	if output, err := fixture.git(t, one, "push", "origin", "main"); err == nil {
		t.Fatalf("push published after key revocation: %s", output)
	}
	after, err := fixture.stores[2].ReadGit(t.Context(), fixture.registry.group, "demo")
	if err != nil || after.References["refs/heads/main"] != before.References["refs/heads/main"] {
		t.Fatalf("revoked push changed the repository: %v", err)
	}
	for entry := range 4 {
		if output, err := fixture.git(t, root, "ls-remote", fixture.url(entry, fixture.registry.group)); err == nil {
			t.Fatalf("revoked key accepted by shard %d: %s", entry, output)
		}
	}
	fixture.registry.mu.Lock()
	defer fixture.registry.mu.Unlock()
	if len(fixture.registry.verified) != 1 || fixture.registry.verified[0] == 0 ||
		fixture.registry.authorized[0] == 0 || fixture.registry.authorized[2] == 0 {
		t.Fatalf("authority calls reached wrong shards: keys=%v ACLs=%v",
			fixture.registry.verified, fixture.registry.authorized,
		)
	}
}

func (f *nativeSSHFixture) dial(t *testing.T, user string, key ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", f.addresses[1])
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		if closeErr := raw.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		return nil, err
	}
	connection, channels, requests, err := ssh.NewClientConn(raw, f.addresses[1], &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(f.host.PublicKey()),
	})
	if err != nil {
		if closeErr := raw.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		return nil, err
	}
	client := ssh.NewClient(connection, channels, requests)
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			t.Error(err)
		}
	})
	return client, nil
}

func TestSSHRejectsNonGitAccess(t *testing.T) {
	t.Parallel()
	fixture := newNativeSSHFixture(t)
	for _, tc := range []struct {
		name, command string
	}{
		{name: "shell command", command: "sh"},
		{name: "archive", command: "git-upload-archive '/team/demo.git'"},
		{name: "command injection", command: "git-upload-pack '/team/demo.git'; id"},
		{name: "peer impersonation", command: "gitone-peer e30"},
		{name: "traversal", command: "git-upload-pack '/../team/demo.git'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := fixture.dial(t, fixture.registry.username, fixture.user)
			if err != nil {
				t.Fatal(err)
			}
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Run(tc.command); err == nil {
				t.Fatal("non-Git command succeeded")
			}
		})
	}
	for _, kind := range []string{"direct-tcpip", "forwarded-tcpip", "auth-agent@openssh.com"} {
		t.Run(kind, func(t *testing.T) {
			client, err := fixture.dial(t, fixture.registry.username, fixture.user)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.OpenChannel(kind, nil); err == nil {
				t.Fatal("forwarding channel accepted")
			}
		})
	}
	for _, request := range []string{"shell", "pty-req", "subsystem", "auth-agent-req@openssh.com"} {
		t.Run(request, func(t *testing.T) {
			client, err := fixture.dial(t, fixture.registry.username, fixture.user)
			if err != nil {
				t.Fatal(err)
			}
			channel, _, err := client.OpenChannel("session", nil)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := channel.SendRequest(request, true, nil)
			if err != nil || accepted {
				t.Fatalf("unexpected request response: accepted=%v error=%v", accepted, err)
			}
		})
	}
	wrong, _ := nativeSSHSigner(t)
	t.Run("host key mismatch", func(t *testing.T) {
		untrusted := *fixture
		untrusted.host = wrong
		if _, err := untrusted.dial(t, fixture.registry.username, fixture.user); err == nil {
			t.Fatal("untrusted SSH server host key accepted")
		}
	})
	for _, tc := range []struct {
		name, user string
		key        ssh.Signer
	}{
		{name: "unknown key", user: fixture.registry.username, key: wrong},
		{name: "wrong username", user: "unknown", key: fixture.user},
		{name: "peer username", user: peerUser, key: fixture.user},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fixture.dial(t, tc.user, tc.key); err == nil {
				t.Fatal("invalid credentials accepted")
			}
		})
	}
	for _, operation := range []string{"key-check", "git"} {
		t.Run("wrong owner peer "+operation, func(t *testing.T) {
			// Connect as a legitimate peer to entry shard 1. The user authority
			// is shard 0, and the group repository is shard 2. Neither peer
			// operation may be forwarded again or accepted by the wrong owner.
			client, err := fixture.dial(t, peerUser, fixture.forward)
			if err != nil {
				t.Fatal(err)
			}
			command, err := encodePeerRequest(peerRequest{
				Operation: operation, Username: fixture.registry.username,
				Key:     fixture.user.PublicKey().Marshal(),
				Command: "git-upload-pack '/" + fixture.registry.group + "/demo.git'",
			})
			if err != nil {
				t.Fatal(err)
			}
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Run(command); err == nil {
				t.Fatal("wrong-owner peer operation was accepted")
			}
		})
	}
}

func TestSSHShutdownClosesIdleConnections(t *testing.T) {
	t.Parallel()
	fixture := newNativeSSHFixture(t)
	client, err := fixture.dial(t, fixture.registry.username, fixture.user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(); err != nil {
		t.Fatal(err)
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(t.Context(), "tcp", fixture.addresses[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	if err := raw.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Reading the banner proves the server has accepted this connection and
	// started a handshake worker, without supplying our own identification.
	reader := bufio.NewReader(raw)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { fixture.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown did not join idle session and handshake workers")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("incomplete handshake connection survived shutdown")
	}
	if _, err := client.NewSession(); err == nil {
		t.Fatal("authenticated connection survived shutdown")
	}
}
