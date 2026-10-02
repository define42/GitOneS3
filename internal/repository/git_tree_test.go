package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestTreeSmallFilesStorageLatency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		lfs    bool
		legacy int
	}{
		{name: "indexed ordinary files"},
		{name: "indexed mixed files", lfs: true},
		{name: "legacy packed mixed files", lfs: true, legacy: 2},
		{name: "legacy loose mixed files", lfs: true, legacy: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, object := treeSmallFilesFixture(t, test.lfs, test.legacy)
			// Synthetic time keeps a real 15-second request budget deterministic
			// without making the regression sleep for every storage round trip.
			synctest.Test(t, func(t *testing.T) {
				observed := &treeReadStore{ObjectStore: store.objects, delay: 25 * time.Millisecond}
				store.objects = observed
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				started := time.Now()
				tree, err := store.Tree(ctx, "alice", "demo", "main", "")
				if err != nil || len(tree.Entries) != 650 {
					t.Fatalf("tree entries = %d, error = %v", len(tree.Entries), err)
				}
				for index, entry := range tree.Entries {
					if entry.Name != fmt.Sprintf("file%04d.txt", index) {
						t.Fatalf("entry order changed: %d = %q", index, entry.Name)
					}
					if test.lfs && index < 2 {
						if entry.LFS == nil || *entry.LFS != object || entry.Size != object.Size {
							t.Fatalf("LFS entry = %+v", entry)
						}
					} else if entry.LFS != nil {
						t.Fatalf("ordinary file treated as LFS: %+v", entry)
					}
				}
				wantReads := int64(651) // Commit, tree, 648 ordinary blobs, one shared pointer blob.
				if !test.lfs {
					wantReads = 2 // The authoritative empty LFS index avoids every blob read.
				}
				if got := observed.payloadReads.Load(); got != wantReads {
					t.Fatalf("payload reads = %d, want %d", got, wantReads)
				}
				peak := observed.peak.Load()
				if peak > maxTreeLFSReads || (test.lfs && peak != maxTreeLFSReads) {
					t.Fatalf("concurrent storage reads = %d", peak)
				}
				if observed.active.Load() != 0 {
					t.Fatal("tree returned before its storage readers exited")
				}
				if elapsed := time.Since(started); elapsed > 3*time.Second {
					t.Fatalf("650-file directory took %s with 25ms storage latency", elapsed)
				}
			})
		})
	}
}

func TestTreePointerReadsStopOnFailure(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"corrupt object", "cancelled request"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, _ := treeSmallFilesFixture(t, true, 0)
			snap, err := store.load(t.Context(), "alice", "demo")
			if err != nil {
				t.Fatal(err)
			}
			blob := GitObject{Type: "blob", Data: []byte("ordinary file 0002")}
			info := snap.manifest.Objects[GitObjectID(blob)]
			synctest.Test(t, func(t *testing.T) {
				observed := &treeReadStore{ObjectStore: store.objects, delay: 25 * time.Millisecond}
				store.objects = observed
				timeout := 15 * time.Second
				want := ErrCorrupt
				if name == "cancelled request" {
					timeout, want = 60*time.Millisecond, context.DeadlineExceeded
				} else {
					observed.corruptKey = "repos/" + snap.metadata.ID + "/" + info.PackKey
					observed.corruptOffset = info.Offset
				}
				ctx, cancel := context.WithTimeout(t.Context(), timeout)
				defer cancel()
				tree, err := store.Tree(ctx, "alice", "demo", "main", "")
				if !errors.Is(err, want) || len(tree.Entries) != 0 {
					t.Fatalf("tree after failure = %+v, %v; want %v", tree, err, want)
				}
				if observed.active.Load() != 0 || observed.peak.Load() > maxTreeLFSReads {
					t.Fatalf("readers after failure: active = %d, peak = %d", observed.active.Load(), observed.peak.Load())
				}
				if reads := observed.payloadReads.Load(); reads < 3 || reads > 20 {
					t.Fatalf("failure did not stop candidate reads promptly: %d", reads)
				}
			})
		})
	}
}

func treeSmallFilesFixture(t *testing.T, lfs bool, legacy int) (*Store, LFSObject) {
	t.Helper()
	store, _, _ := lfsFixture(t)
	files := make(map[string][]byte, 650)
	for index := range 650 {
		files[fmt.Sprintf("file%04d.txt", index)] = []byte(fmt.Sprintf("ordinary file %04d", index))
	}
	var object LFSObject
	if lfs {
		data := []byte("content in LFS\n")
		var err error
		object, err = store.LFSUpload(
			t.Context(), "alice", "demo", lfsOID(data), int64(len(data)), bytes.NewReader(data), lfsLimits(), allowLFS,
		)
		if err != nil {
			t.Fatal(err)
		}
		// Two paths share one Git pointer blob; inspect that blob only once.
		files["file0000.txt"] = lfsPointer(object.OID, object.Size).Data
		files["file0001.txt"] = files["file0000.txt"]
	}
	publishBrowserFiles(t, store, files)
	if legacy != 0 {
		makeLegacyTreeManifest(t, store, legacy)
	}
	return store, object
}

func makeLegacyTreeManifest(t *testing.T, store *Store, schema int) {
	t.Helper()
	snap, err := store.load(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	manifest := snap.manifest
	manifest.SchemaVersion, manifest.LFS = schema, nil
	if schema == 1 {
		manifest.Objects = map[string]objectInfo{}
		for id, info := range snap.manifest.Objects {
			content, err := store.object(t.Context(), snap, id, info.Type)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.putObject(t.Context(), snap.metadata.ID, info.Type, content, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	key, err := store.putSnapshot(t.Context(), snap.metadata.ID, "manifest", manifest)
	if err != nil {
		t.Fatal(err)
	}
	state := snap.state
	state.Generation++
	state.PackManifest = key
	if err := store.repositories.CompareAndSwapState(t.Context(), snap.metadata.ID, snap.version, state); err != nil {
		t.Fatal(err)
	}
}

type treeReadStore struct {
	storage.ObjectStore
	delay         time.Duration
	corruptKey    string
	corruptOffset int64
	payloadReads  atomic.Int64
	active        atomic.Int64
	peak          atomic.Int64
}

func (s *treeReadStore) wait(ctx context.Context) error {
	s.payloadReads.Add(1)
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for peak := s.peak.Load(); active > peak; peak = s.peak.Load() {
		if s.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *treeReadStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if strings.Contains(key, "/objects/") || strings.Contains(key, "/packs/") {
		if err := s.wait(ctx); err != nil {
			return nil, storage.ObjectInfo{}, err
		}
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *treeReadStore) GetRange(
	ctx context.Context,
	key string,
	offset, length int64,
) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := s.wait(ctx); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	if key == s.corruptKey && offset == s.corruptOffset {
		return io.NopCloser(strings.NewReader("corrupt Git object")), storage.ObjectInfo{}, nil
	}
	return s.ObjectStore.GetRange(ctx, key, offset, length)
}
