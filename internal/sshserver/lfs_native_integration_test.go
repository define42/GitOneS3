//go:build integration

package sshserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/securecookie"
	"golang.org/x/crypto/ssh"

	"github.com/define42/GitOneS3/internal/auth"
	"github.com/define42/GitOneS3/internal/config"
	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/lfs"
	"github.com/define42/GitOneS3/internal/protocol"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type nativeLFSProvider struct{}

func (nativeLFSProvider) AuthorizationURL(string, string, string) string { return "" }
func (nativeLFSProvider) Exchange(context.Context, string, string, string) (auth.Identity, error) {
	return auth.Identity{}, errors.New("OIDC is not used by this SSH fixture")
}

func TestNativeLFSSSHLifecycle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"git", "git-lfs", "ssh"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	options := unitServerOptions(t)
	objects := storage.NewMemoryStore()
	repos, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repos.Create(t.Context(), "alice", repository.CreateInput{
		Name: "demo", CreatedBy: "alice", AuthorName: "Alice", AuthorEmail: "alice@example.test", InitializeReadme: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A legacy namespace fixture exercises the same durable user ownership
	// check used after a real OIDC registration.
	user := []byte(`{"subject":"alice-id","email":"alice@example.test"}`)
	if _, err := objects.Put(t.Context(), "auth/users/alice.json", bytes.NewReader(user), int64(len(user)), storage.PutOptions{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	gitHandler, err := gittransport.New(repos)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpServer := httptest.NewTLSServer(mux)
	t.Cleanup(httpServer.Close)
	lfsHandler, err := lfs.New(repos, lfs.Options{PublicURL: httpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	hash, block := bytes.Repeat([]byte{61}, 64), bytes.Repeat([]byte{62}, 32)
	service, err := auth.New(auth.Options{
		Config: config.Auth{Enabled: true, PublicURL: httpServer.URL,
			GoogleClientID: "fixture", GoogleClientSecret: "fixture",
			CookieHashKey: base64.StdEncoding.EncodeToString(hash), CookieBlockKey: base64.StdEncoding.EncodeToString(block)},
		Router: options.Router, Store: objects, Provider: nativeLFSProvider{},
		Next: protocol.NewHandler(gitHandler, lfsHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	var uploads, downloads, batches, verifications atomic.Int32
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/info/lfs/objects/") {
			switch r.Method {
			case http.MethodPut:
				uploads.Add(1)
			case http.MethodGet:
				downloads.Add(1)
			case http.MethodPost:
				if strings.HasSuffix(r.URL.Path, "/verify") {
					verifications.Add(1)
				}
			}
		}
		if strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch") && batches.Add(1) == 1 {
			// Simulate one action that expired while queued. The native client
			// must request a fresh batch instead of attempting its stale PUT.
			first := httptest.NewRecorder()
			service.ServeHTTP(first, r)
			var batch map[string]any
			if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &batch) != nil {
				t.Errorf("initial LFS batch failed: %d", first.Code)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			for _, object := range batch["objects"].([]any) {
				actions := object.(map[string]any)["actions"].(map[string]any)
				for _, action := range actions {
					action.(map[string]any)["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
				}
			}
			maps.Copy(w.Header(), first.Header())
			if err := json.NewEncoder(w).Encode(batch); err != nil {
				t.Error(err)
			}
			return
		}
		service.ServeHTTP(w, r)
	}))
	// Register the actual SSH key through the public management API.
	userSigner, userPrivate := nativeSSHSigner(t)
	codec := securecookie.New(hash, block).SetSerializer(securecookie.JSONEncoder{})
	session, err := codec.Encode("__Host-gitone-session", map[string]any{
		"Username": "alice", "Identity": auth.Identity{Subject: "alice-id", Email: "alice@example.test"},
		"Origin": httpServer.URL, "CSRF": "fixture-csrf", "Expires": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyBody, err := json.Marshal(map[string]string{"name": "Native LFS", "publicKey": string(ssh.MarshalAuthorizedKey(userSigner.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/users/alice/ssh-keys", bytes.NewReader(keyBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", httpServer.URL)
	request.Header.Set("X-CSRF-Token", "fixture-csrf")
	request.AddCookie(&http.Cookie{Name: "__Host-gitone-session", Value: session,
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("register key: %d %s", response.Code, response.Body.String())
	}
	options.Authority, options.Git = service, gitHandler
	sshFixture := startUnitSSHOptions(t, options)
	directory := t.TempDir()
	fixture := &nativeSSHFixture{key: filepath.Join(directory, "identity"), knownHost: filepath.Join(directory, "known_hosts")}
	if err := os.WriteFile(fixture.key, userPrivate, 0600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(sshFixture.address)
	if err != nil {
		t.Fatal(err)
	}
	knownHost := "[" + host + "]:" + port + " " + string(ssh.MarshalAuthorizedKey(options.HostKey.PublicKey()))
	if err := os.WriteFile(fixture.knownHost, []byte(knownHost), 0600); err != nil {
		t.Fatal(err)
	}
	certificate := filepath.Join(directory, "http-ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: httpServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		configuration := []string{
			"-c", "http.sslCAInfo=" + certificate,
			"-c", "filter.lfs.clean=git-lfs clean -- %f",
			"-c", "filter.lfs.smudge=git-lfs smudge -- %f",
			"-c", "filter.lfs.process=git-lfs filter-process",
			"-c", "filter.lfs.required=true",
		}
		return fixture.gitRun(t, dir, append(configuration, args...)...)
	}
	remote := "ssh://alice@" + sshFixture.address + "/alice/demo.git"
	run(directory, "clone", remote, "writer")
	writer := filepath.Join(directory, "writer")
	run(writer, "lfs", "install", "--local")
	run(writer, "lfs", "track", "*.bin")
	content := bytes.Repeat([]byte("native SSH Git LFS payload\n"), (10<<20)/26)
	path := filepath.Join(writer, "payload.bin")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	run(writer, "add", ".gitattributes", "payload.bin")
	run(writer, "commit", "-m", "Upload LFS content through GitOne")
	run(writer, "push", "origin", "main")
	if uploads.Load() == 0 {
		t.Fatal("SSH push did not stream LFS content through GitOne HTTP")
	}
	if batches.Load() < 2 || verifications.Load() != 0 {
		t.Fatalf("expired action was not refreshed or optional verify was advertised: batches=%d verifies=%d", batches.Load(), verifications.Load())
	}
	run(directory, "clone", remote, "reader")
	reader := filepath.Join(directory, "reader")
	// #nosec G304 -- The fixture creates this checkout beneath t.TempDir.
	got, err := os.ReadFile(filepath.Join(reader, "payload.bin"))
	if err != nil || !bytes.Equal(got, content) || downloads.Load() == 0 {
		t.Fatalf("native LFS clone content mismatch: bytes=%d downloads=%d err=%v", len(got), downloads.Load(), err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	object, err := repos.LFSStat(t.Context(), "alice", "demo", digest)
	if err != nil || object.Size != int64(len(content)) {
		t.Fatalf("verified LFS object missing: %+v %v", object, err)
	}
	pointer := run(writer, "show", "HEAD:payload.bin")
	if !strings.Contains(pointer, "oid sha256:"+digest) || len(pointer) > 200 {
		t.Fatalf("Git commit did not retain a small LFS pointer: %s", pointer)
	}
	run(reader, "lfs", "fsck")
	run(reader, "fsck", "--full", "--strict")
	if _, err := repos.CheckIntegrity(t.Context(), "alice", "demo"); err != nil {
		t.Fatal(err)
	}
	t.Logf("native SSH LFS uploaded and cloned %d bytes through GitOne HTTP; uploads=%d downloads=%d", len(content), uploads.Load(), downloads.Load())
}
