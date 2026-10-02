package repository

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestBrowseLoadsOneFreshManifest(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	observed := &browseManifestStore{ObjectStore: objects}
	store.objects = observed
	for _, test := range []struct {
		name, path string
		history    bool
	}{
		{name: "directory and readme"},
		{name: "file", path: "README.md"},
		{name: "history", history: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed.reads.Store(0)
			page, err := store.Browse(t.Context(), "alice", "demo", "", test.path, test.history)
			if err != nil || page.Metadata.Name != "demo" || len(page.Branches) != 1 {
				t.Fatalf("browse = %+v, %v", page, err)
			}
			commit := page.Branches[0].Commit
			switch {
			case test.history:
				if len(page.Commits) != 1 || page.Commits[0].ID != commit || page.Tree != nil || page.Blob != nil {
					t.Fatalf("history = %+v", page)
				}
			case test.path != "":
				if page.Blob == nil || page.Blob.Commit != commit || !strings.Contains(page.Blob.Content, "# demo") || page.Tree != nil {
					t.Fatalf("file = %+v", page)
				}
			default:
				if page.Tree == nil || page.Tree.Commit != commit || len(page.Tree.Entries) != 1 || page.Readme == nil || page.Readme.Commit != commit || !strings.Contains(page.Readme.Content, "# demo") {
					t.Fatalf("directory = %+v", page)
				}
			}
			if reads := observed.reads.Load(); reads != 1 {
				t.Fatalf("manifest reads = %d, want one per complete page", reads)
			}
		})
	}
	// A later request still validates the immutable manifest bytes. There is no
	// shared cache that could conceal external corruption after a successful read.
	snap, err := store.load(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Put(t.Context(), "repos/"+snap.metadata.ID+"/"+snap.state.PackManifest, strings.NewReader("{}"), 2, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Browse(t.Context(), "alice", "demo", "", "", false); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest = %v", err)
	}
}

func TestBrowsePinsGenerationDuringPublication(t *testing.T) {
	t.Parallel()
	store, objects := newTestStore(t)
	if _, err := store.Create(t.Context(), "alice", createInput("demo", true)); err != nil {
		t.Fatal(err)
	}
	observed := &browseManifestStore{ObjectStore: objects}
	store.objects = observed
	observed.afterRead = func() {
		publishBrowserFiles(t, store, map[string][]byte{"README.md": []byte("New generation\n")})
	}
	page, err := store.Browse(t.Context(), "alice", "demo", "", "", false)
	if err != nil || page.Tree == nil || page.Readme == nil || len(page.Branches) != 1 {
		t.Fatalf("browse during publication = %+v, %v", page, err)
	}
	if page.Tree.Commit != page.Branches[0].Commit || page.Readme.Commit != page.Tree.Commit || !strings.Contains(page.Readme.Content, "# demo") {
		t.Fatalf("page mixed published generations: %+v", page)
	}
	next, err := store.Browse(t.Context(), "alice", "demo", "", "", false)
	if err != nil || next.Readme == nil || next.Readme.Content != "New generation\n" || next.Readme.Commit == page.Readme.Commit {
		t.Fatalf("next request did not observe publication: %+v, %v", next, err)
	}
}

func TestBrowseEmptyValidationAndMissingPreview(t *testing.T) {
	t.Parallel()
	store, objects, metadata := lfsFixture(t)
	page, err := store.Browse(t.Context(), "alice", "demo", "", "", false)
	if err != nil || !page.Metadata.IsEmpty || page.Tree == nil || len(page.Tree.Entries) != 0 || len(page.Branches) != 0 {
		t.Fatalf("empty repository = %+v, %v", page, err)
	}
	for _, test := range []struct {
		ref, path string
		want      error
	}{
		{path: "../README.md", want: ErrInvalid},
		{ref: "main^", want: ErrInvalid},
		{ref: "missing", want: ErrNotFound},
		{path: "README.md", want: ErrNotFound},
	} {
		if _, err := store.Browse(t.Context(), "alice", "demo", test.ref, test.path, false); !errors.Is(err, test.want) {
			t.Errorf("browse ref %q, path %q = %v, want %v", test.ref, test.path, err, test.want)
		}
	}
	data := "README in LFS\n"
	object, err := store.LFSUpload(t.Context(), "alice", "demo", lfsOID([]byte(data)), int64(len(data)), strings.NewReader(data), lfsLimits(), allowLFS)
	if err != nil {
		t.Fatal(err)
	}
	publishBrowserFiles(t, store, map[string][]byte{"README.md": lfsPointer(object.OID, object.Size).Data})
	key := lfsRecordKey(metadata.ID, object.OID)
	info, err := objects.Head(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(t.Context(), key, info.Version); err != nil {
		t.Fatal(err)
	}
	page, err = store.Browse(t.Context(), "alice", "demo", "", "", false)
	if err != nil || page.Tree == nil || len(page.Tree.Entries) != 1 || page.Readme != nil {
		t.Fatalf("directory with unavailable preview = %+v, %v", page, err)
	}
	if _, err := store.Browse(t.Context(), "alice", "demo", "", "README.md", false); !errors.Is(err, ErrLFSMissing) {
		t.Fatalf("explicit missing file = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Browse(ctx, "alice", "demo", "", "", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled browser = %v", err)
	}
}

type browseManifestStore struct {
	storage.ObjectStore
	reads     atomic.Int64
	afterRead func()
}

func (s *browseManifestStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, key)
	if strings.Contains(key, "-manifest-") {
		s.reads.Add(1)
		if hook := s.afterRead; hook != nil {
			s.afterRead = nil
			hook()
		}
	}
	return body, info, err
}
