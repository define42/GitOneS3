package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/lfs"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

func TestAPIRepositoryLFSBrowserAccess(t *testing.T) {
	t.Parallel()
	s := testService(t, 0, storage.NewMemoryStore(), &fakeProvider{}, nil)
	handler, err := lfs.New(s.repositories, lfs.Options{PublicURL: s.origin})
	if err != nil {
		t.Fatal(err)
	}
	s.next = handler
	owner, ownerCSRF := groupSession(t, s, "alice", "alice-id")
	reader, readerCSRF := groupSession(t, s, "bob", "bob-id")
	outsider, _ := groupSession(t, s, "eve", "eve-id")
	ownerCheck := repositoryRequestChecker(t, s, owner, ownerCSRF)
	ownerCheck("POST", "/api/v1/groups/acme", "", http.StatusCreated)
	ownerCheck("POST", "/api/v1/repos/acme", `{"name":"project"}`, http.StatusCreated)
	ownerCheck("POST", "/api/v1/groups/acme/invitations", `{"userId":"google:bob-id","role":"reader"}`, http.StatusOK)
	repositoryRequestChecker(t, s, reader, readerCSRF)("POST", "/api/v1/groups/acme/invitations/accept", "", http.StatusOK)

	const content = "hello from LFS\n"
	object := publishBrowserLFSText(t, s.repositories, content)
	paths := []string{
		"/api/v1/repos/acme/project/tree?ref=main",
		"/api/v1/repos/acme/project/blob?ref=main&path=hello.txt",
		"/acme/project.git/info/lfs/objects/" + object.OID,
	}
	request := func(t *testing.T, path string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("private LFS response is cacheable: %v", w.Header())
		}
		if want != http.StatusOK && (strings.Contains(w.Body.String(), content) || strings.Contains(w.Body.String(), object.OID)) {
			t.Fatalf("unauthorized response exposed LFS content: %s", w.Body.String())
		}
		return w
	}
	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		status int
	}{
		{name: "owner", cookie: owner, status: http.StatusOK},
		{name: "group reader", cookie: reader, status: http.StatusOK},
		{name: "outsider", cookie: outsider, status: http.StatusForbidden},
		{name: "anonymous", status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			treeResponse := request(t, paths[0], test.cookie, test.status)
			blobResponse := request(t, paths[1], test.cookie, test.status)
			download := request(t, paths[2], test.cookie, test.status)
			if test.status != http.StatusOK {
				return
			}
			var tree repository.Tree
			if err := json.Unmarshal(treeResponse.Body.Bytes(), &tree); err != nil {
				t.Fatal(err)
			}
			if len(tree.Entries) != 1 || tree.Entries[0].Name != "hello.txt" ||
				tree.Entries[0].LFS == nil || *tree.Entries[0].LFS != object || tree.Entries[0].Size != object.Size {
				t.Fatalf("LFS directory metadata: %+v", tree)
			}
			var blob repository.Blob
			if err := json.Unmarshal(blobResponse.Body.Bytes(), &blob); err != nil {
				t.Fatal(err)
			}
			if blob.Content != content || blob.LFS == nil || *blob.LFS != object || blob.Size != object.Size || blob.IsBinary {
				t.Fatalf("LFS browser preview: %+v", blob)
			}
			if download.Body.String() != content || download.Header().Get("Content-Type") != "application/octet-stream" ||
				download.Header().Get("X-Content-Type-Options") != "nosniff" || download.Header().Get("Location") != "" {
				t.Fatalf("LFS download was not served through GitOne: headers=%v body=%q", download.Header(), download.Body.String())
			}
		})
	}

	ownerCheck("DELETE", "/api/v1/groups/acme/members", `{"userId":"google:bob-id"}`, http.StatusOK)
	t.Run("revoked reader", func(t *testing.T) {
		for _, path := range paths {
			request(t, path, reader, http.StatusForbidden)
		}
	})
}

func publishBrowserLFSText(t *testing.T, store *repository.Store, content string) repository.LFSObject {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(digest[:])
	allow := func(context.Context) error { return nil }
	object, err := store.LFSUpload(t.Context(), "acme", "project", oid, int64(len(content)), strings.NewReader(content),
		repository.LFSLimits{MaxObjectBytes: 1024, MaxRepositoryBytes: 1024}, allow)
	if err != nil {
		t.Fatal(err)
	}
	pointer := repository.GitObject{Type: "blob", Data: []byte(fmt.Sprintf(
		"version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, object.Size))}
	pointerID, err := hex.DecodeString(repository.GitObjectID(pointer))
	if err != nil {
		t.Fatal(err)
	}
	tree := repository.GitObject{Type: "tree", Data: append([]byte("100644 hello.txt\x00"), pointerID...)}
	commit := repository.GitObject{Type: "commit", Data: []byte("tree " + repository.GitObjectID(tree) +
		"\nauthor Alice <alice@example.com> 1700000000 +0000\ncommitter Alice <alice@example.com> 1700000000 +0000\n\nAdd LFS file\n")}
	base, err := store.ReadGitReferences(t.Context(), "acme", "project")
	if err != nil {
		t.Fatal(err)
	}
	objects := make(map[string]repository.GitObject, 3)
	for _, item := range []repository.GitObject{pointer, tree, commit} {
		objects[repository.GitObjectID(item)] = item
	}
	if err := store.PublishGit(t.Context(), base,
		[]repository.RefUpdate{{Name: "refs/heads/main", New: repository.GitObjectID(commit)}}, objects, allow); err != nil {
		t.Fatal(err)
	}
	return object
}
