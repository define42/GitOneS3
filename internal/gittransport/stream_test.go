package gittransport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type uploadReadStore struct {
	storage.ObjectStore
	packGets, rangeGets int
	packBytes           int64
}

func (s *uploadReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, key)
	if strings.Contains(key, "/packs/") {
		s.packGets++
		s.packBytes += info.Size
	}
	return body, info, err
}

func (s *uploadReadStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/packs/") {
		s.rangeGets++
		s.packBytes += length
	}
	return s.ObjectStore.GetRange(ctx, key, offset, length)
}

func TestPrepareUploadSparseIncrementalAndDenseClone(t *testing.T) {
	t.Parallel()
	objects := &uploadReadStore{ObjectStore: storage.NewMemoryStore()}
	store, err := repository.New(objects)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "sparse", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	base, err := store.ReadGitReferences(t.Context(), "alice", "sparse")
	if err != nil {
		t.Fatal(err)
	}
	fixture, old, head := benchmarkSnapshot(2)
	if err := store.PublishGit(t.Context(), base, []repository.RefUpdate{{Name: "refs/heads/main", New: head}}, fixture.Objects, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	handler, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		haves []string
	}{
		{name: "incremental", haves: []string{old}},
		{name: "clone"},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects.packGets, objects.rangeGets, objects.packBytes = 0, 0, 0
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/alice/sparse.git/git-upload-pack", bytes.NewReader(uploadRequest([]string{head}, test.haves, true, true)))
			request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("upload status=%d: %s", response.Code, response.Body.String())
			}
			got := uploadPackObjects(t, response.Body.Bytes(), true)
			if len(test.haves) > 0 {
				if len(got) != 1 || !bytes.Equal(got[head].Data, fixture.Objects[head].Data) {
					t.Fatalf("incremental response did not contain exactly the new commit: %d objects", len(got))
				}
				if objects.packGets != 0 || objects.rangeGets != 3 || objects.packBytes > 4096 {
					t.Fatalf("tiny incremental response downloaded existing blobs: full=%d ranges=%d bytes=%d", objects.packGets, objects.rangeGets, objects.packBytes)
				}
				return
			}
			if len(got) != len(fixture.Objects) || objects.packGets != 1 || objects.rangeGets != 3 {
				t.Fatalf("dense clone objects=%d full=%d ranges=%d", len(got), objects.packGets, objects.rangeGets)
			}
		})
	}
}

func TestReceiveThinPackPlansSparseAndDenseBases(t *testing.T) {
	for _, test := range []struct {
		name  string
		bases int
	}{
		{name: "sparse", bases: 1},
		{name: "dense", bases: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &uploadReadStore{ObjectStore: storage.NewMemoryStore()}
			store, err := repository.New(objects)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(t.Context(), "alice", repository.CreateInput{Name: "thin", CreatedBy: "alice"}); err != nil {
				t.Fatal(err)
			}
			base, err := store.ReadGitReferences(t.Context(), "alice", "thin")
			if err != nil {
				t.Fatal(err)
			}
			fixture, _, head := benchmarkSnapshot(4)
			if err := store.PublishGit(t.Context(), base, []repository.RefUpdate{{Name: "refs/heads/main", New: head}}, fixture.Objects, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			var pack packFixture
			var commands strings.Builder
			updated := map[string]string{}
			for _, id := range slices.Sorted(maps.Keys(fixture.Objects)) {
				object := fixture.Objects[id]
				if object.Type != "blob" || len(updated) == test.bases {
					continue
				}
				changed := bytes.Clone(object.Data)
				changed[0] ^= 0xff
				newID := repository.GitObjectID(repository.GitObject{Type: "blob", Data: changed})
				ref := fmt.Sprintf("refs/tags/changed-%d", len(updated))
				capability := ""
				if len(updated) == 0 {
					capability = "\x00report-status"
				}
				commands.WriteString(pkt(zeroID + " " + newID + " " + ref + capability + "\n"))
				updated[ref] = newID
				instructions := binary.AppendUvarint(nil, uint64(len(changed)))
				instructions = binary.AppendUvarint(instructions, uint64(len(changed)))
				instructions = append(instructions, 1, changed[0], 0xf1, 1, 0xff, 0xff, 0x0f)
				rawID, err := hex.DecodeString(id)
				if err != nil {
					t.Fatal(err)
				}
				pack.add(t, 7, rawID, instructions)
			}
			commands.WriteString("0000")
			body := append([]byte(commands.String()), pack.finish()...)
			handler, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			objects.packGets, objects.rangeGets, objects.packBytes = 0, 0, 0
			ctx := WithWriteAuthorization(t.Context(), func(context.Context) error { return nil })
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/alice/thin.git/git-receive-pack", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "unpack ok\n") || strings.Contains(response.Body.String(), "ng ") {
				t.Fatalf("thin push status=%d: %s", response.Code, response.Body.String())
			}
			if test.bases == 1 {
				if objects.packGets != 0 || objects.packBytes > 2<<20 {
					t.Fatalf("sparse thin base downloaded unrelated blobs: full=%d ranges=%d bytes=%d", objects.packGets, objects.rangeGets, objects.packBytes)
				}
			} else if objects.packGets != 1 || objects.rangeGets != 6 || objects.packBytes > 5<<20 {
				t.Fatalf("dense thin bases not coalesced: full=%d ranges=%d bytes=%d", objects.packGets, objects.rangeGets, objects.packBytes)
			}
			published, err := store.ReadGitReferences(t.Context(), "alice", "thin")
			if err != nil {
				t.Fatal(err)
			}
			for ref, id := range updated {
				if published.References[ref] != id || !strings.Contains(response.Body.String(), "ok "+ref+"\n") {
					t.Fatalf("thin pack reference %s was not published correctly", ref)
				}
			}
		})
	}
}
