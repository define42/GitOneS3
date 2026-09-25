package sshserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

type unitAuthority struct {
	key        []byte
	revoked    atomic.Bool
	readOnly   atomic.Bool
	verified   atomic.Int32
	authorized atomic.Int32
}

func (a *unitAuthority) VerifySSHKey(ctx context.Context, username string, key []byte) (auth.SSHPrincipal, error) {
	a.verified.Add(1)
	if ctx.Err() != nil || username != "alice" || !bytes.Equal(key, a.key) || a.revoked.Load() {
		return auth.SSHPrincipal{}, repository.ErrForbidden
	}
	return auth.SSHPrincipal{Username: username, Identity: auth.Identity{Subject: "alice-id"}}, nil
}

func (a *unitAuthority) AuthorizeSSH(
	ctx context.Context,
	principal auth.SSHPrincipal,
	namespace string,
	write bool,
) error {
	a.authorized.Add(1)
	if ctx.Err() != nil || principal.Username != "alice" || namespace != "alice" || (write && a.readOnly.Load()) {
		return repository.ErrForbidden
	}
	return nil
}

func unitSigner(t *testing.T, seed byte) ssh.Signer {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func unitServerOptions(t *testing.T) Options {
	t.Helper()
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(1, parser)
	if err != nil {
		t.Fatal(err)
	}
	store, err := repository.New(storage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "alice", AuthorName: "Alice", AuthorEmail: "alice@example.test", InitializeReadme: true,
	}); err != nil {
		t.Fatal(err)
	}
	git, err := gittransport.New(store)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Address: "127.0.0.1:0", Router: router,
		HostKey: unitSigner(t, 1), ForwardKey: unitSigner(t, 2),
		Authority: &unitAuthority{key: unitSigner(t, 3).PublicKey().Marshal()}, Git: git,
		PeerAddress: func(shard.ShardID) (string, error) { return "", errors.New("unexpected peer request") },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	base := unitServerOptions(t)
	for _, test := range []struct {
		name   string
		change func(*Options)
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "default logger", change: func(o *Options) { o.Logger = nil }, valid: true},
		{name: "missing address", change: func(o *Options) { o.Address = "" }},
		{name: "missing router", change: func(o *Options) { o.Router = nil }},
		{name: "missing resolver", change: func(o *Options) { o.PeerAddress = nil }},
		{name: "missing host key", change: func(o *Options) { o.HostKey = nil }},
		{name: "missing forwarding key", change: func(o *Options) { o.ForwardKey = nil }},
		{name: "missing authority", change: func(o *Options) { o.Authority = nil }},
		{name: "missing Git handler", change: func(o *Options) { o.Git = nil }},
		{name: "invalid shard", change: func(o *Options) { o.LocalShard = 1 }},
		{name: "shared role key", change: func(o *Options) { o.ForwardKey = o.HostKey }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := base
			if test.change != nil {
				test.change(&options)
			}
			server, err := New(options)
			if (err == nil) != test.valid || (server != nil) != test.valid {
				t.Fatalf("valid=%v server=%v error=%v", test.valid, server != nil, err)
			}
		})
	}
}

func TestLoadSigner(t *testing.T) {
	t.Parallel()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	plain, err := ssh.MarshalPrivateKey(private, "test fixture")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(private, "test fixture", []byte("test-only-password"))
	if err != nil {
		t.Fatal(err)
	}
	ecdsaPrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongType, err := ssh.MarshalPrivateKey(ecdsaPrivate, "unsupported server key")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{name: "Ed25519", data: pem.EncodeToMemory(plain), valid: true},
		{name: "encrypted", data: pem.EncodeToMemory(encrypted)},
		{name: "other algorithm", data: pem.EncodeToMemory(wrongType)},
		{name: "invalid", data: []byte("not a private key")},
		{name: "public only", data: ssh.MarshalAuthorizedKey(unitSigner(t, 1).PublicKey())},
		{name: "oversized", data: bytes.Repeat([]byte{'x'}, 16385)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			signer, err := LoadSigner(path)
			if (err == nil) != test.valid || (signer != nil) != test.valid {
				t.Fatalf("valid=%v signer=%v error=%v", test.valid, signer != nil, err)
			}
			if test.valid && !bytes.Equal(signer.PublicKey().Marshal(), unitSigner(t, 1).PublicKey().Marshal()) {
				t.Fatal("loaded a different key")
			}
		})
	}
	if _, err := LoadSigner(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := LoadSigner(t.TempDir()); err == nil {
		t.Fatal("directory accepted as private key")
	}
}

func TestParseCommand(t *testing.T) {
	t.Parallel()
	server, err := New(unitServerOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		for _, prefix := range []string{"", "/"} {
			t.Run(service+prefix, func(t *testing.T) {
				t.Parallel()
				parsed, err := server.parseCommand(service + " '" + prefix + "alice/demo.git'")
				if err != nil || parsed.service != service || parsed.namespace != "alice" || parsed.repository != "demo" {
					t.Fatalf("parsed=%+v error=%v", parsed, err)
				}
			})
		}
	}
	for _, test := range []struct{ name, command string }{
		{name: "shell", command: "sh"},
		{name: "archive", command: "git-upload-archive 'alice/demo.git'"},
		{name: "unquoted", command: "git-upload-pack alice/demo.git"},
		{name: "double quoted", command: `git-upload-pack "alice/demo.git"`},
		{name: "extra space", command: "git-upload-pack  'alice/demo.git'"},
		{name: "tab separator", command: "git-upload-pack\t'alice/demo.git'"},
		{name: "missing suffix", command: "git-upload-pack 'alice/demo'"},
		{name: "absolute double slash", command: "git-upload-pack '//alice/demo.git'"},
		{name: "path traversal", command: "git-upload-pack 'alice/../demo.git'"},
		{name: "namespace traversal", command: "git-upload-pack '../demo.git'"},
		{name: "percent encoded", command: "git-upload-pack '%61lice/demo.git'"},
		{name: "uppercase namespace", command: "git-upload-pack 'Alice/demo.git'"},
		{name: "reserved namespace", command: "git-upload-pack 'auth/demo.git'"},
		{name: "option", command: "git-upload-pack '--strict'"},
		{name: "extra command", command: "git-upload-pack 'alice/demo.git'; id"},
		{name: "quote injection", command: "git-upload-pack 'alice/demo.git' && 'id'"},
		{name: "substitution", command: "git-upload-pack 'alice/$(id).git'"},
		{name: "newline", command: "git-upload-pack 'alice/demo.git'\n"},
		{name: "nul", command: "git-upload-pack 'alice/demo\x00.git'"},
		{name: "backslash", command: "git-upload-pack 'alice\\demo.git'"},
		{name: "oversized", command: "git-upload-pack 'alice/" + strings.Repeat("a", 257) + ".git'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := server.parseCommand(test.command); err == nil {
				t.Fatalf("unsafe command accepted: %q", test.command)
			}
		})
	}
}

func TestPeerRequestAndOutputBounds(t *testing.T) {
	t.Parallel()
	request := peerRequest{Operation: "git", Username: "alice", Key: unitSigner(t, 3).PublicKey().Marshal(),
		Command: "git-upload-pack 'alice/demo.git'"}
	command, err := encodePeerRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePeerRequest(command)
	if err != nil || decoded.Operation != request.Operation || decoded.Username != request.Username ||
		decoded.Command != request.Command || !bytes.Equal(decoded.Key, request.Key) {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	for _, test := range []struct{ name, command string }{
		{name: "missing prefix", command: "anything"},
		{name: "invalid base64", command: "gitone-peer !!!"},
		{name: "invalid JSON", command: "gitone-peer " + base64.RawStdEncoding.EncodeToString([]byte("bad"))},
		{name: "unknown field", command: "gitone-peer " + base64.RawStdEncoding.EncodeToString([]byte(`{"admin":true}`))},
		{name: "trailing value", command: "gitone-peer " + base64.RawStdEncoding.EncodeToString([]byte(`{} {}`))},
		{name: "oversized command", command: "gitone-peer " + strings.Repeat("a", maxCommand)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodePeerRequest(test.command); err == nil {
				t.Fatal("invalid peer request accepted")
			}
		})
	}
	request.Key = make([]byte, 8193)
	command, err = encodePeerRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePeerRequest(command); err == nil {
		t.Fatal("oversized peer key accepted")
	}
	request.Command = strings.Repeat("x", maxCommand)
	if _, err := encodePeerRequest(request); err == nil {
		t.Fatal("oversized encoded peer request accepted")
	}
	var output boundedOutput
	if n, err := output.Write(make([]byte, 8192)); n != 8192 || err != nil {
		t.Fatalf("bounded write failed: %d %v", n, err)
	}
	if n, err := output.Write([]byte{1}); n != 0 || err == nil || output.Len() != 8192 {
		t.Fatalf("output limit bypassed: n=%d size=%d err=%v", n, output.Len(), err)
	}
}

type unitSSHFixture struct {
	server  *Server
	address string
	cancel  context.CancelFunc
	done    <-chan struct{}
}

func startUnitSSH(t *testing.T) unitSSHFixture {
	t.Helper()
	return startUnitSSHOptions(t, unitServerOptions(t))
}

func startUnitSSHOptions(t *testing.T, options Options) unitSSHFixture {
	t.Helper()
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ctx, listener); err != nil {
			t.Errorf("serve SSH: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("SSH server did not join workers on cancellation")
		}
	})
	return unitSSHFixture{server: server, address: listener.Addr().String(), cancel: cancel, done: done}
}

func (f unitSSHFixture) dial(t *testing.T, username string, methods ...ssh.AuthMethod) (*ssh.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", f.address)
	if err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	config := &ssh.ClientConfig{User: username, Auth: methods,
		HostKeyCallback: ssh.FixedHostKey(f.server.options.HostKey.PublicKey())}
	conn, channels, requests, err := ssh.NewClientConn(raw, f.address, config)
	if err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	client := ssh.NewClient(conn, channels, requests)
	t.Cleanup(func() { _ = client.Close() }) // Closing twice is harmless for a read-only test connection.
	return client, nil
}

type mismatchedSigner struct {
	ssh.Signer
	public ssh.PublicKey
}

func (s mismatchedSigner) PublicKey() ssh.PublicKey { return s.public }

func TestSSHHandshakeRequiresPossessionAndCorrectIdentity(t *testing.T) {
	t.Parallel()
	fixture := startUnitSSH(t)
	user := unitSigner(t, 3)
	for _, test := range []struct {
		name, username string
		methods        []ssh.AuthMethod
		valid          bool
	}{
		{name: "registered key", username: "alice", methods: []ssh.AuthMethod{ssh.PublicKeys(user)}, valid: true},
		{name: "trusted peer", username: peerUser,
			methods: []ssh.AuthMethod{ssh.PublicKeys(fixture.server.options.ForwardKey)}, valid: true},
		{name: "no authentication", username: "alice"},
		{name: "password", username: "alice", methods: []ssh.AuthMethod{ssh.Password("not-supported")}},
		{name: "unknown key", username: "alice", methods: []ssh.AuthMethod{ssh.PublicKeys(unitSigner(t, 4))}},
		{name: "wrong username", username: "bob", methods: []ssh.AuthMethod{ssh.PublicKeys(user)}},
		{name: "reserved username", username: "auth", methods: []ssh.AuthMethod{ssh.PublicKeys(user)}},
		{name: "peer impersonation", username: peerUser, methods: []ssh.AuthMethod{ssh.PublicKeys(user)}},
		{name: "forwarding key is not user key", username: "alice",
			methods: []ssh.AuthMethod{ssh.PublicKeys(fixture.server.options.ForwardKey)}},
		{name: "approved public key without its private key", username: "alice",
			methods: []ssh.AuthMethod{ssh.PublicKeys(mismatchedSigner{Signer: unitSigner(t, 4), public: user.PublicKey()})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, err := fixture.dial(t, test.username, test.methods...)
			if (err == nil) != test.valid || (client != nil) != test.valid {
				t.Fatalf("valid=%v client=%v err=%v", test.valid, client != nil, err)
			}
		})
	}
}

func TestSSHRejectsNonGitCapabilities(t *testing.T) {
	t.Parallel()
	fixture := startUnitSSH(t)
	for _, test := range []struct {
		name    string
		request func(*ssh.Client, *ssh.Session) error
	}{
		{name: "shell", request: func(_ *ssh.Client, s *ssh.Session) error { return s.Shell() }},
		{name: "SFTP", request: func(_ *ssh.Client, s *ssh.Session) error { return s.RequestSubsystem("sftp") }},
		{name: "environment", request: func(_ *ssh.Client, s *ssh.Session) error { return s.Setenv("GIT_PROTOCOL", "version=2") }},
		{name: "terminal", request: func(_ *ssh.Client, s *ssh.Session) error {
			return s.RequestPty("xterm", 80, 24, ssh.TerminalModes{})
		}},
		{name: "second session", request: func(c *ssh.Client, _ *ssh.Session) error {
			extra, err := c.NewSession()
			if extra != nil {
				return errors.Join(err, extra.Close())
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
			if err != nil {
				t.Fatal(err)
			}
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			if err := test.request(client, session); err == nil {
				t.Fatal("non-Git capability accepted")
			}
		})
	}
	t.Run("port and agent forwarding", func(t *testing.T) {
		t.Parallel()
		client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"direct-tcpip", "forwarded-tcpip", "auth-agent@openssh.com", "unknown"} {
			channel, _, err := client.OpenChannel(kind, nil)
			if channel != nil {
				if err := channel.Close(); err != nil {
					t.Error(err)
				}
			}
			if err == nil {
				t.Fatalf("channel type %s accepted", kind)
			}
		}
		ok, _, err := client.SendRequest("tcpip-forward", true, nil)
		if err != nil || ok {
			t.Fatalf("remote forwarding request: accepted=%v err=%v", ok, err)
		}
	})
}

func TestSSHExecAuthorizationAndErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, command string
		revoke        bool
		readOnly      bool
		valid         bool
	}{
		{name: "fetch", command: "git-upload-pack 'alice/demo.git'", valid: true},
		{name: "empty push", command: "git-receive-pack 'alice/demo.git'", valid: true},
		{name: "read-only fetch", command: "git-upload-pack 'alice/demo.git'", readOnly: true, valid: true},
		{name: "read-only push", command: "git-receive-pack 'alice/demo.git'", readOnly: true},
		{name: "revoked after handshake", command: "git-upload-pack 'alice/demo.git'", revoke: true},
		{name: "unknown repository", command: "git-upload-pack 'alice/missing.git'"},
		{name: "other namespace", command: "git-upload-pack 'bob/demo.git'"},
		{name: "shell command", command: "id"},
		{name: "forged peer command", command: "gitone-peer e30"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := startUnitSSH(t)
			client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
			if err != nil {
				t.Fatal(err)
			}
			authority := fixture.server.options.Authority.(*unitAuthority)
			authority.revoked.Store(test.revoke)
			authority.readOnly.Store(test.readOnly)
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			session.Stdin = strings.NewReader("0000")
			output, err := session.CombinedOutput(test.command)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v output=%q error=%v", test.valid, output, err)
			}
			if test.valid {
				if !bytes.Contains(output, []byte("refs/heads/main")) || authority.verified.Load() < 2 || authority.authorized.Load() != 1 {
					t.Fatalf("missing advertisement or fresh authorization: %q verified=%d authorized=%d",
						output, authority.verified.Load(), authority.authorized.Load())
				}
				return
			}
			exit, ok := errors.AsType[*ssh.ExitError](err)
			if !ok || exit.ExitStatus() != 1 || !bytes.Contains(output, []byte("GitOne: access denied")) {
				t.Fatalf("missing generic failure or exit code: output=%q err=%v", output, err)
			}
			if bytes.Contains(output, []byte("refs/heads/main")) {
				t.Fatal("denied session leaked repository advertisement")
			}
		})
	}
}

func TestSSHRejectsMalformedAndOversizedExecRequests(t *testing.T) {
	t.Parallel()
	fixture := startUnitSSH(t)
	client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "truncated", payload: []byte{0, 0, 0, 10, 'x'}},
		{name: "oversized", payload: ssh.Marshal(struct{ Command string }{Command: strings.Repeat("x", maxCommand)})},
		{name: "trailing fields", payload: append(ssh.Marshal(struct{ Command string }{Command: "id"}), 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted, err := session.SendRequest("exec", true, test.payload)
			if err != nil || accepted {
				t.Fatalf("invalid exec accepted=%v err=%v", accepted, err)
			}
		})
	}
	// Rejected channel requests do not consume the one permitted Git command.
	session.Stdin = strings.NewReader("0000")
	output, err := session.Output("git-upload-pack 'alice/demo.git'")
	if err != nil || !bytes.Contains(output, []byte("refs/heads/main")) {
		t.Fatalf("valid exec after rejected requests: output=%q err=%v", output, err)
	}
}

func TestSSHPeerKeyChecksAndCommands(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		operation string
		username  string
		command   string
		valid     bool
	}{
		{name: "key authority", operation: "key-check", username: "alice", valid: true},
		{name: "delegated Git", operation: "git", username: "alice", command: "git-upload-pack 'alice/demo.git'", valid: true},
		{name: "unknown operation", operation: "shell", username: "alice"},
		{name: "unknown user", operation: "key-check", username: "bob"},
		{name: "empty identity", operation: "key-check"},
		{name: "nested delegation", operation: "git", username: "alice", command: "gitone-peer e30"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := startUnitSSH(t)
			client, err := fixture.dial(t, peerUser, ssh.PublicKeys(fixture.server.options.ForwardKey))
			if err != nil {
				t.Fatal(err)
			}
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			if test.operation == "git" {
				session.Stdin = strings.NewReader("0000")
			}
			command, err := encodePeerRequest(peerRequest{Operation: test.operation, Username: test.username,
				Key: unitSigner(t, 3).PublicKey().Marshal(), Command: test.command})
			if err != nil {
				t.Fatal(err)
			}
			output, err := session.Output(command)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v output=%q err=%v", test.valid, output, err)
			}
			if test.valid && test.operation == "key-check" {
				var principal auth.SSHPrincipal
				if err := json.Unmarshal(output, &principal); err != nil || principal.Username != "alice" || principal.Identity.Subject != "alice-id" {
					t.Fatalf("invalid key authority result: %+v %v", principal, err)
				}
			}
		})
	}
}

func TestSSHPeerWrongOwnerNeverForwardsAgain(t *testing.T) {
	t.Parallel()
	options := unitServerOptions(t)
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	options.Router, err = shard.NewRouter(2, parser)
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := options.Router.Owner("alice"); err != nil || owner != 1 {
		t.Fatalf("wrong test user owner: %d %v", owner, err)
	}
	var forwards atomic.Int32
	options.PeerAddress = func(shard.ShardID) (string, error) {
		forwards.Add(1)
		return "", errors.New("must not forward peer requests")
	}
	fixture := startUnitSSHOptions(t, options)
	for _, operation := range []string{"key-check", "git"} {
		t.Run(operation, func(t *testing.T) {
			client, err := fixture.dial(t, peerUser, ssh.PublicKeys(options.ForwardKey))
			if err != nil {
				t.Fatal(err)
			}
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			command, err := encodePeerRequest(peerRequest{Operation: operation, Username: "alice",
				Key: unitSigner(t, 3).PublicKey().Marshal(), Command: "git-upload-pack 'alice/demo.git'"})
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Run(command); err == nil {
				t.Fatal("wrong-owner peer request accepted")
			}
			if forwards.Load() != 0 || options.Authority.(*unitAuthority).verified.Load() != 0 {
				t.Fatal("wrong-owner peer request was forwarded or verified locally")
			}
		})
	}
}

func TestServeCancellationClosesIncompleteAndActiveConnections(t *testing.T) {
	t.Parallel()
	fixture := startUnitSSH(t)
	var dialer net.Dialer
	incomplete, err := dialer.DialContext(t.Context(), "tcp", fixture.address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = incomplete.Close() })
	client, err := fixture.dial(t, "alice", ssh.PublicKeys(unitSigner(t, 3)))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := session.Start("git-upload-pack 'alice/demo.git'"); err != nil {
		t.Fatal(err)
	}
	fixture.cancel()
	select {
	case <-fixture.done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close incomplete and active SSH sessions")
	}
	if err := client.Wait(); err == nil {
		t.Fatal("canceled connection ended successfully")
	}
}

func TestRunCancellationAndListenerErrors(t *testing.T) {
	t.Parallel()
	server, err := New(unitServerOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.Run(ctx); err != nil {
		t.Fatalf("canceled Run: %v", err)
	}
	server.options.Address = "not a valid listen address"
	if err := server.Run(t.Context()); err == nil {
		t.Fatal("invalid listener address accepted")
	}
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(t.Context(), listener); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener error not propagated: %v", err)
	}
}
