//go:build integration

package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/protocol"
	"github.com/define42/GitOneS3/internal/proxy"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/shard"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestGitPATPersonalWorkflow(t *testing.T) {
	t.Parallel()
	cluster := newGitTestCluster(t)
	alice := cluster.user(t, "alice")
	cluster.api(t, alice, "POST", "/api/v1/repos/alice", `{"name":"project","initializeReadme":true}`, 201)
	cluster.api(t, alice, "POST", "/api/v1/repos/alice", `{"name":"other","initializeReadme":true}`, 201)
	writeToken := cluster.token(t, alice, "write", "alice/project")
	readToken := cluster.token(t, alice, "read", "alice/project")
	remote := cluster.servers[0].URL + "/alice/project.git"
	discovery := remote + "/info/refs?service=git-upload-pack"
	cluster.status(t, discovery, "", "", 401)
	cluster.status(t, discovery, "alice", "invalid-token", 401)
	cluster.status(t, discovery, "bob", writeToken.secret, 401)
	cluster.status(t, cluster.servers[0].URL+"/alice/other.git/info/refs?service=git-upload-pack", "alice", writeToken.secret, 403)
	cluster.status(t, remote+"/info/refs?service=git-receive-pack", "alice", readToken.secret, 403)

	work := t.TempDir()
	writer, reader := filepath.Join(work, "writer"), filepath.Join(work, "reader")
	cluster.git(t, work, alice.name, writeToken.secret, "clone", remote, writer)
	cluster.git(t, work, alice.name, readToken.secret, "clone", remote, reader)
	contents := "# project\n\nWritten through native Git.\n"
	gitTestWrite(t, filepath.Join(writer, "README.md"), contents)
	cluster.git(t, writer, alice.name, writeToken.secret, "add", "README.md")
	cluster.git(t, writer, alice.name, writeToken.secret, "commit", "-m", "Update README through Git")
	cluster.git(t, writer, alice.name, writeToken.secret, "push", "origin", "HEAD:main")
	cluster.git(t, reader, alice.name, readToken.secret, "pull", "--ff-only", "origin", "main")
	if got := gitTestRead(t, filepath.Join(reader, "README.md")); got != contents {
		t.Fatalf("pulled README = %q", got)
	}
	cluster.assertBlob(t, alice, "alice/project", contents)
	var history struct {
		Commits []struct {
			ID string `json:"id"`
		} `json:"commits"`
	}
	response := cluster.api(t, alice, "GET", "/api/v1/repos/alice/project/commits?ref=main", "", 200)
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(cluster.git(t, writer, alice.name, writeToken.secret, "rev-parse", "HEAD"))
	if len(history.Commits) < 2 || history.Commits[0].ID != head {
		t.Fatal("browser history does not match the pushed Git commit")
	}

	gitTestWrite(t, filepath.Join(reader, "README.md"), "read-only write attempt\n")
	cluster.git(t, reader, alice.name, readToken.secret, "add", "README.md")
	cluster.git(t, reader, alice.name, readToken.secret, "commit", "-m", "Denied write")
	cluster.gitDenied(t, reader, alice.name, readToken.secret, "push", "origin", "HEAD:main")
	cluster.assertBlob(t, alice, "alice/project", contents)
	cluster.api(t, alice, "DELETE", "/api/v1/users/alice/tokens/"+writeToken.id, "", 204)
	cluster.status(t, discovery, alice.name, writeToken.secret, 401)
	cluster.gitDenied(t, writer, alice.name, writeToken.secret, "fetch", "origin")
}

func TestGitPATGroupAuthorityAndMembership(t *testing.T) {
	t.Parallel()
	cluster := newGitTestCluster(t)
	alice, bob := cluster.user(t, "alice"), cluster.user(t, "bob")
	if cluster.owner(t, "alice") == cluster.owner(t, "acme") {
		t.Fatal("fixture must separate the token authority from the group shard")
	}
	cluster.api(t, bob, "POST", "/api/v1/groups/acme", "", 201)
	invite := `{"userId":"google:alice-id","role":"developer"}`
	cluster.api(t, bob, "POST", "/api/v1/groups/acme/invitations", invite, 200)
	cluster.api(t, alice, "POST", "/api/v1/groups/acme/invitations/accept", "", 200)
	cluster.api(t, alice, "POST", "/api/v1/repos/acme", `{"name":"shared","initializeReadme":true}`, 201)
	token := cluster.token(t, alice, "write", "acme/shared")
	// Enter through the user's shard: Git forwards to the group, then token
	// verification calls back to the authority without forwarding the Git body.
	remote := cluster.servers[cluster.owner(t, "alice")].URL + "/acme/shared.git"
	work := t.TempDir()
	checkout := filepath.Join(work, "shared")
	cluster.git(t, work, alice.name, token.secret, "clone", remote, checkout)
	contents := "# shared\n\nA cross-shard group push.\n"
	gitTestWrite(t, filepath.Join(checkout, "README.md"), contents)
	cluster.git(t, checkout, alice.name, token.secret, "add", "README.md")
	cluster.git(t, checkout, alice.name, token.secret, "commit", "-m", "Group update")
	cluster.git(t, checkout, alice.name, token.secret, "push", "origin", "HEAD:main")
	cluster.assertBlob(t, bob, "acme/shared", contents)
	cluster.api(t, bob, "PUT", "/api/v1/groups/acme/members", `{"userId":"google:alice-id","role":"reader"}`, 200)
	cluster.status(t, remote+"/info/refs?service=git-upload-pack", alice.name, token.secret, 200)
	cluster.status(t, remote+"/info/refs?service=git-receive-pack", alice.name, token.secret, 403)
	cluster.gitDenied(t, checkout, alice.name, token.secret, "push", "origin", "HEAD:main")
	cluster.api(t, bob, "PUT", "/api/v1/groups/acme/members", `{"userId":"google:alice-id","role":"developer"}`, 200)
	cluster.api(t, alice, "DELETE", "/api/v1/users/alice/tokens/"+token.id, "", 204)
	cluster.status(t, remote+"/info/refs?service=git-upload-pack", alice.name, token.secret, 401)
	cluster.gitDenied(t, checkout, alice.name, token.secret, "fetch", "origin")
	token = cluster.token(t, alice, "write", "acme/shared")
	cluster.api(t, bob, "DELETE", "/api/v1/groups/acme/members", `{"userId":"google:alice-id"}`, 200)
	cluster.status(t, remote+"/info/refs?service=git-upload-pack", alice.name, token.secret, 403)
	cluster.gitDenied(t, checkout, alice.name, token.secret, "fetch", "origin")

	// Restoring membership does not mint a new token. If its issuing shard then
	// disappears, the repository shard must not use a cached positive result.
	cluster.api(t, bob, "POST", "/api/v1/groups/acme/invitations", invite, 200)
	cluster.api(t, alice, "POST", "/api/v1/groups/acme/invitations/accept", "", 200)
	groupRemote := cluster.servers[cluster.owner(t, "acme")].URL + "/acme/shared.git"
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, token.secret, 200)
	cluster.servers[cluster.owner(t, "alice")].Close()
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, token.secret, 503)
	cluster.gitDenied(t, checkout, alice.name, token.secret, "ls-remote", groupRemote)
}

func TestGitPATEmptyRepositoryFirstPush(t *testing.T) {
	t.Parallel()
	cluster := newGitTestCluster(t)
	alice := cluster.user(t, "alice")
	cluster.api(t, alice, "POST", "/api/v1/repos/alice", `{"name":"empty"}`, 201)
	token := cluster.token(t, alice, "write", "alice/empty")
	remote := cluster.servers[0].URL + "/alice/empty.git"
	work := t.TempDir()
	checkout := filepath.Join(work, "empty")
	cluster.git(t, work, alice.name, token.secret, "clone", remote, checkout)
	contents := "# empty\n\nThe first pushed commit.\n"
	gitTestWrite(t, filepath.Join(checkout, "README.md"), contents)
	cluster.git(t, checkout, alice.name, token.secret, "add", "README.md")
	cluster.git(t, checkout, alice.name, token.secret, "commit", "-m", "First native commit")
	cluster.git(t, checkout, alice.name, token.secret, "push", "origin", "HEAD:main")
	cluster.assertBlob(t, alice, "alice/empty", contents)
	cloned := filepath.Join(work, "fresh")
	cluster.git(t, work, alice.name, token.secret, "clone", remote, cloned)
	if got := gitTestRead(t, filepath.Join(cloned, "README.md")); got != contents {
		t.Fatal("fresh clone did not contain the first pushed commit")
	}
}

func TestGitPATAllRepositoriesFutureAccessAndRevocation(t *testing.T) {
	t.Parallel()
	cluster := newGitTestCluster(t)
	alice, bob := cluster.user(t, "alice"), cluster.user(t, "bob")
	// Issue credentials before any repositories or groups exist. Broad selection
	// follows future access, but must never replace the account's live ACL.
	writeToken := cluster.allToken(t, alice, "write")
	readToken := cluster.allToken(t, alice, "read")
	cluster.api(t, alice, "POST", "/api/v1/repos/alice", `{"name":"future","initializeReadme":true}`, 201)
	cluster.api(t, bob, "POST", "/api/v1/repos/bob", `{"name":"private","initializeReadme":true}`, 201)
	cluster.api(t, bob, "POST", "/api/v1/groups/outsiders", "", 201)
	cluster.api(t, bob, "POST", "/api/v1/repos/outsiders", `{"name":"private","initializeReadme":true}`, 201)
	if cluster.owner(t, "alice") == cluster.owner(t, "acme") {
		t.Fatal("fixture must separate the token authority from the future group shard")
	}
	cluster.api(t, bob, "POST", "/api/v1/groups/acme", "", 201)
	cluster.api(t, bob, "POST", "/api/v1/repos/acme", `{"name":"future","initializeReadme":true}`, 201)

	work := t.TempDir()
	for _, test := range []struct{ name, repository string }{
		{name: "other personal namespace", repository: "bob/private"},
		{name: "outsider group", repository: "outsiders/private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := cluster.servers[0].URL + "/" + test.repository + ".git"
			cluster.status(t, remote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 403)
			cluster.status(t, remote+"/info/refs?service=git-receive-pack", alice.name, writeToken.secret, 403)
			cluster.gitDenied(t, work, alice.name, writeToken.secret, "ls-remote", remote)
		})
	}
	personalRemote := cluster.servers[0].URL + "/alice/future.git"
	writer, reader := filepath.Join(work, "personal-writer"), filepath.Join(work, "personal-reader")
	cluster.git(t, work, alice.name, writeToken.secret, "clone", personalRemote, writer)
	cluster.git(t, work, alice.name, readToken.secret, "clone", personalRemote, reader)
	contents := "# future\n\nCreated after the all-repository token.\n"
	gitTestWrite(t, filepath.Join(writer, "README.md"), contents)
	cluster.git(t, writer, alice.name, writeToken.secret, "add", "README.md")
	cluster.git(t, writer, alice.name, writeToken.secret, "commit", "-m", "All-repository personal update")
	cluster.git(t, writer, alice.name, writeToken.secret, "push", "origin", "HEAD:main")
	cluster.git(t, reader, alice.name, readToken.secret, "pull", "--ff-only", "origin", "main")
	if got := gitTestRead(t, filepath.Join(reader, "README.md")); got != contents {
		t.Fatal("all-repository read token did not pull the future repository update")
	}
	cluster.assertBlob(t, alice, "alice/future", contents)
	cluster.status(t, personalRemote+"/info/refs?service=git-receive-pack", alice.name, readToken.secret, 403)
	cluster.gitDenied(t, reader, alice.name, readToken.secret, "push", "origin", "HEAD:main")

	groupRemote := cluster.servers[cluster.owner(t, "alice")].URL + "/acme/future.git"
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 403)
	cluster.api(t, bob, "POST", "/api/v1/groups/acme/invitations", `{"userId":"google:alice-id","role":"developer"}`, 200)
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 403)
	cluster.api(t, alice, "POST", "/api/v1/groups/acme/invitations/accept", "", 200)
	groupCheckout := filepath.Join(work, "group")
	cluster.git(t, work, alice.name, writeToken.secret, "clone", groupRemote, groupCheckout)
	groupContents := "# future\n\nThe account joined this group after token issuance.\n"
	gitTestWrite(t, filepath.Join(groupCheckout, "README.md"), groupContents)
	cluster.git(t, groupCheckout, alice.name, writeToken.secret, "add", "README.md")
	cluster.git(t, groupCheckout, alice.name, writeToken.secret, "commit", "-m", "All-repository group update")
	cluster.git(t, groupCheckout, alice.name, writeToken.secret, "push", "origin", "HEAD:main")
	cluster.assertBlob(t, bob, "acme/future", groupContents)
	cluster.git(t, groupCheckout, alice.name, readToken.secret, "fetch", "origin")
	cluster.status(t, groupRemote+"/info/refs?service=git-receive-pack", alice.name, readToken.secret, 403)

	cluster.api(t, bob, "PUT", "/api/v1/groups/acme/members", `{"userId":"google:alice-id","role":"reader"}`, 200)
	cluster.git(t, groupCheckout, alice.name, writeToken.secret, "fetch", "origin")
	cluster.status(t, groupRemote+"/info/refs?service=git-receive-pack", alice.name, writeToken.secret, 403)
	cluster.gitDenied(t, groupCheckout, alice.name, writeToken.secret, "push", "origin", "HEAD:main")
	cluster.api(t, bob, "DELETE", "/api/v1/groups/acme/members", `{"userId":"google:alice-id"}`, 200)
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 403)
	cluster.gitDenied(t, groupCheckout, alice.name, writeToken.secret, "fetch", "origin")

	cluster.api(t, alice, "DELETE", "/api/v1/users/alice/tokens/"+writeToken.id, "", 204)
	cluster.status(t, personalRemote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 401)
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, writeToken.secret, 401)
	cluster.gitDenied(t, writer, alice.name, writeToken.secret, "fetch", "origin")
	cluster.expireToken(t, alice, readToken)
	cluster.status(t, personalRemote+"/info/refs?service=git-upload-pack", alice.name, readToken.secret, 401)
	cluster.status(t, groupRemote+"/info/refs?service=git-upload-pack", alice.name, readToken.secret, 401)
	cluster.gitDenied(t, reader, alice.name, readToken.secret, "fetch", "origin")
}

type gitTestCluster struct {
	servers  [2]*httptest.Server
	services [2]*Service
	handlers [2]http.Handler
	router   *shard.Router
	gitPath  string
	secrets  []string
}

type gitTestUser struct {
	name   string
	cookie *http.Cookie
	csrf   string
}

type gitTestToken struct{ id, secret string }

func newGitTestCluster(t *testing.T) *gitTestCluster {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("native Git is required for integration tests")
	}
	parser, err := shard.NewParser(shard.DefaultPathPolicy())
	if err != nil {
		t.Fatal(err)
	}
	router, err := shard.NewRouter(2, parser)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &gitTestCluster{router: router, gitPath: gitPath, secrets: []string{}}
	for index := range cluster.servers {
		cluster.servers[index] = httptest.NewUnstartedServer(http.NotFoundHandler())
		t.Cleanup(cluster.servers[index].Close)
	}
	resolver := resolverFunc(func(id shard.ShardID) (*url.URL, error) {
		if int(id) >= len(cluster.servers) {
			return nil, fmt.Errorf("unknown test shard %d", id)
		}
		return url.Parse("http://" + cluster.servers[id].Listener.Addr().String())
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	for index, server := range cluster.servers {
		objects := storage.NewMemoryStore()
		repositories, err := repository.New(objects)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := gittransport.New(repositories)
		if err != nil {
			t.Fatal(err)
		}
		service, err := New(Options{
			Config: testConfig(), LocalShard: shard.ShardID(index), Router: router,
			Store: objects, Provider: &fakeProvider{}, Next: protocol.NewHandler(engine, nil),
			TokenResolver: resolver, TokenTransport: transport,
		})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := proxy.NewHandler(proxy.HandlerOptions{
			LocalShard: shard.ShardID(index), Router: service, Resolver: resolver,
			Next: service, Transport: transport,
		})
		if err != nil {
			t.Fatal(err)
		}
		cluster.services[index], cluster.handlers[index] = service, handler
		server.Config.Handler = handler
	}
	for _, server := range cluster.servers {
		server.Start()
	}
	return cluster
}

func (c *gitTestCluster) owner(t *testing.T, namespace string) shard.ShardID {
	t.Helper()
	owner, err := c.router.Owner(namespace)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func (c *gitTestCluster) user(t *testing.T, username string) gitTestUser {
	t.Helper()
	service := c.services[c.owner(t, username)]
	if err := service.bindUser(t.Context(), username, Identity{Subject: username + "-id", Email: username + "@example.invalid"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := groupSession(t, service, username, username+"-id")
	return gitTestUser{name: username, cookie: cookie, csrf: csrf}
}

func (c *gitTestCluster) api(t *testing.T, user gitTestUser, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := groupRequest(c.handlers[0], method, path, body, user.cookie, user.csrf)
	if response.Code != want {
		// Token creation can return a secret even if its status is unexpected.
		t.Fatalf("%s %s: status %d, want %d", method, path, response.Code, want)
	}
	return response
}

func (c *gitTestCluster) token(t *testing.T, user gitTestUser, permission, scope string) gitTestToken {
	t.Helper()
	return c.issueToken(t, user, map[string]any{
		"name": "native-git-integration", "permission": permission,
		"repositories": []string{scope}, "expiresInDays": 30,
	})
}

func (c *gitTestCluster) allToken(t *testing.T, user gitTestUser, permission string) gitTestToken {
	t.Helper()
	return c.issueToken(t, user, map[string]any{
		"name": "native-git-all-repositories", "permission": permission,
		"allRepositories": true, "expiresInDays": 30,
	})
}

func (c *gitTestCluster) issueToken(t *testing.T, user gitTestUser, payload map[string]any) gitTestToken {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := c.api(t, user, "POST", "/api/v1/users/"+user.name+"/tokens", string(body), 201)
	var created struct {
		Token    string `json:"token"`
		Metadata struct {
			ID              string   `json:"id"`
			AllRepositories bool     `json:"allRepositories"`
			Repositories    []string `json:"repositories"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal("token response could not be decoded")
	}
	if created.Token == "" || created.Metadata.ID == "" {
		t.Fatal("token response omitted credentials or metadata")
	}
	expectAll, _ := payload["allRepositories"].(bool)
	if created.Metadata.AllRepositories != expectAll || (expectAll && len(created.Metadata.Repositories) != 0) {
		t.Fatal("token response did not preserve the requested repository selection")
	}
	c.secrets = append(c.secrets, created.Token, base64.StdEncoding.EncodeToString([]byte(user.name+":"+created.Token)))
	return gitTestToken{id: created.Metadata.ID, secret: created.Token}
}

func (c *gitTestCluster) expireToken(t *testing.T, user gitTestUser, token gitTestToken) {
	t.Helper()
	// Seed an already-expired authority record without sleeping or changing
	// process-wide time. The subsequent assertions use real HTTP/Git clients.
	service := c.services[c.owner(t, user.name)]
	record, version, err := service.loadToken(t.Context(), user.name, token.id)
	if err != nil {
		t.Fatal(err)
	}
	record.Metadata.CreatedAt = time.Now().Add(-48 * time.Hour)
	record.Metadata.ExpiresAt = time.Now().Add(-24 * time.Hour)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal("could not encode expired token fixture")
	}
	if _, err := service.store.Put(t.Context(), tokenKey(user.name, token.id), bytes.NewReader(data), int64(len(data)),
		storage.PutOptions{IfMatch: version}); err != nil {
		t.Fatal("could not persist expired token fixture")
	}
}

func (c *gitTestCluster) status(t *testing.T, target, username, secret string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if username != "" {
		request.SetBasicAuth(username, secret)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(c.redact(err.Error()))
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != want {
		t.Fatalf("Git discovery: status %d, want %d", response.StatusCode, want)
	}
	if want == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == "" {
		t.Error("Git authentication challenge missing")
	}
}

func (c *gitTestCluster) assertBlob(t *testing.T, user gitTestUser, repo, contents string) {
	t.Helper()
	response := c.api(t, user, "GET", "/api/v1/repos/"+repo+"/blob?ref=main&path=README.md", "", 200)
	var blob repository.Blob
	if err := json.Unmarshal(response.Body.Bytes(), &blob); err != nil {
		t.Fatal(err)
	}
	if blob.Content != contents || blob.IsBinary {
		t.Fatal("browser blob does not match the pushed file")
	}
}

func (c *gitTestCluster) git(t *testing.T, dir, username, secret string, args ...string) string {
	t.Helper()
	output, err := c.gitCommand(t, dir, username, secret, args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", args[0], err, c.redact(output))
	}
	return output
}

func (c *gitTestCluster) gitDenied(t *testing.T, dir, username, secret string, args ...string) {
	t.Helper()
	if output, err := c.gitCommand(t, dir, username, secret, args...); err == nil {
		t.Fatalf("git %s unexpectedly succeeded: %s", args[0], c.redact(output))
	}
}

func (c *gitTestCluster) gitCommand(t *testing.T, dir, username, secret string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	// Only the ephemeral helper's environment contains the PAT; never a URL,
	// command argument, persistent credential store, or inherited trace output.
	helper := `!f() { test "$1" = get || exit 0; printf 'username=%s\npassword=%s\n' "$GITONE_TEST_USERNAME" "$GITONE_TEST_PAT"; }; f`
	options := []string{"-c", "credential.helper=", "-c", "credential.helper=" + helper,
		"-c", "user.name=GitOne Integration", "-c", "user.email=git-test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main"}
	command := exec.CommandContext(ctx, c.gitPath, append(options, args...)...)
	command.Dir = dir
	command.WaitDelay = 5 * time.Second
	command.Env = []string{}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "GCM_") || strings.HasPrefix(key, "GITONE_TEST_") {
			continue
		}
		command.Env = append(command.Env, item)
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GITONE_TEST_USERNAME="+username, "GITONE_TEST_PAT="+secret,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	output, err := command.CombinedOutput()
	return string(output), err
}

func (c *gitTestCluster) redact(value string) string {
	for _, secret := range c.secrets {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

func gitTestWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitTestRead(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
