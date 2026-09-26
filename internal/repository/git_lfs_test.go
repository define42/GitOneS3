package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

func TestBrowseLFSFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		data   []byte
		binary bool
		large  bool
	}{
		{name: "text", data: []byte("hello\n")},
		{name: "empty"},
		{name: "binary", data: []byte{'a', 0, 255}, binary: true},
		{name: "preview limit", data: bytes.Repeat([]byte{'a'}, maxObjectBytes)},
		{name: "large", data: bytes.Repeat([]byte{'a'}, maxObjectBytes+1), large: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, objects, metadata := lfsFixture(t)
			object := LFSObject{OID: lfsOID(test.data), Size: int64(len(test.data))}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", object.OID, object.Size,
				bytes.NewReader(test.data), lfsLimits(), allowLFS); err != nil {
				t.Fatal(err)
			}
			ordinary := bytes.Repeat([]byte{'b'}, maxLFSPointerBytes)
			publishBrowserFiles(t, store, map[string][]byte{
				"hello.txt":    lfsPointer(object.OID, object.Size).Data,
				"ordinary.txt": ordinary,
			})
			observed := &browseLFSStore{ObjectStore: objects}
			store.objects = observed
			// A directory listing may inspect small Git pointer candidates, but
			// must not download either LFS payloads or ordinary larger blobs.
			ordinaryID := GitObjectID(GitObject{Type: "blob", Data: ordinary})
			observed.forbiddenKey = objectKey(metadata.ID, ordinaryID)
			tree, err := store.Tree(t.Context(), "alice", "demo", "", "")
			if err != nil || len(tree.Entries) != 2 {
				t.Fatalf("tree = %+v, %v", tree, err)
			}
			entry := tree.Entries[0]
			if entry.Name != "hello.txt" || entry.Type != "file" || entry.LFS == nil ||
				*entry.LFS != object || entry.Size != object.Size || observed.payloadReads != 0 {
				t.Fatalf("LFS entry = %+v, payload reads = %d", entry, observed.payloadReads)
			}
			if tree.Entries[1].LFS != nil || tree.Entries[1].Size != int64(len(ordinary)) {
				t.Fatalf("ordinary entry = %+v", tree.Entries[1])
			}
			blob, err := store.Blob(t.Context(), "alice", "demo", "", "hello.txt")
			if err != nil || blob.LFS == nil || *blob.LFS != object || blob.Size != object.Size ||
				blob.IsBinary != test.binary || blob.TooLarge != test.large {
				t.Fatalf("LFS blob metadata = %+v, %v", blob, err)
			}
			if test.binary || test.large {
				if blob.Content != "" {
					t.Fatal("binary or large LFS file returned preview content")
				}
			} else if blob.Content != string(test.data) {
				t.Fatal("LFS preview did not return the actual stored content")
			}
			wantReads := 1
			if test.large {
				wantReads = 0
			}
			if observed.payloadReads != wantReads {
				t.Fatalf("LFS payload reads = %d, want %d", observed.payloadReads, wantReads)
			}
			observed.forbiddenKey = ""
			blob, err = store.Blob(t.Context(), "alice", "demo", "", "ordinary.txt")
			if err != nil || blob.LFS != nil || blob.TooLarge || blob.Content != string(ordinary) {
				t.Fatalf("ordinary blob changed: %+v, %v", blob, err)
			}
		})
	}
}

func TestBrowseLFSRejectsCorruptContent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		body    string
		missing bool
		mutate  bool
	}{
		{name: "wrong digest", body: "wrong\n"},
		{name: "short body", body: "hello"},
		{name: "unbounded body", body: strings.Repeat("x", maxObjectBytes*2)},
		{name: "missing payload", missing: true},
		{name: "changed version", mutate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, objects, metadata := lfsFixture(t)
			data := []byte("hello\n")
			object := LFSObject{OID: lfsOID(data), Size: int64(len(data))}
			if _, err := store.LFSUpload(t.Context(), "alice", "demo", object.OID, object.Size,
				bytes.NewReader(data), lfsLimits(), allowLFS); err != nil {
				t.Fatal(err)
			}
			publishBrowserFiles(t, store, map[string][]byte{"hello.txt": lfsPointer(object.OID, object.Size).Data})
			record, err := store.readLFSRecord(t.Context(), metadata.ID, object.OID)
			if err != nil {
				t.Fatal(err)
			}
			key := "repos/" + metadata.ID + "/" + record.Key
			if test.missing {
				if err := objects.Delete(t.Context(), key, record.Version); err != nil {
					t.Fatal(err)
				}
			}
			if test.mutate {
				if _, err := objects.Put(t.Context(), key, strings.NewReader("wrong\n"), 6, storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reader := &browseLFSBody{Reader: strings.NewReader(test.body)}
			observed := &browseLFSStore{ObjectStore: objects}
			if !test.missing && !test.mutate {
				observed.replacement = reader
			}
			store.objects = observed
			blob, err := store.Blob(t.Context(), "alice", "demo", "", "hello.txt")
			if !errors.Is(err, ErrCorrupt) || blob.Content != "" {
				t.Fatalf("corrupt LFS preview = %+v, %v", blob, err)
			}
			if observed.replacement != nil && (!reader.closed || reader.bytesRead > int(object.Size)+1) {
				t.Fatalf("preview stream closed = %v, bytes read = %d", reader.closed, reader.bytesRead)
			}
		})
	}
}

func publishBrowserFiles(t *testing.T, store *Store, files map[string][]byte) {
	t.Helper()
	base, err := store.ReadGitReferences(t.Context(), "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]GitObject{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var tree []byte
	for _, name := range names {
		object := GitObject{Type: "blob", Data: files[name]}
		id := GitObjectID(object)
		objects[id] = object
		tree = append(tree, treeBytes(t, "100644", name, id)...)
	}
	treeObject := GitObject{Type: "tree", Data: tree}
	treeID := GitObjectID(treeObject)
	objects[treeID] = treeObject
	commit := GitObject{Type: "commit", Data: []byte("tree " + treeID +
		"\nauthor Alice <alice@example.com> 1700000000 +0000\ncommitter Alice <alice@example.com> 1700000000 +0000\n\nFiles\n")}
	commitID := GitObjectID(commit)
	objects[commitID] = commit
	if err := store.PublishGit(t.Context(), base, []RefUpdate{{Name: "refs/heads/main", New: commitID}}, objects, allowLFS); err != nil {
		t.Fatal(err)
	}
}

type browseLFSStore struct {
	storage.ObjectStore
	forbiddenKey string
	payloadReads int
	replacement  io.ReadCloser
}

func (s *browseLFSStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if key == s.forbiddenKey {
		return nil, storage.ObjectInfo{}, errors.New("directory listing downloaded a larger Git blob")
	}
	body, info, err := s.ObjectStore.Get(ctx, key)
	if err != nil || !strings.Contains(key, "/lfs/objects/") {
		return body, info, err
	}
	s.payloadReads++
	if s.replacement != nil {
		if err := body.Close(); err != nil {
			return nil, info, err
		}
		body = s.replacement
	}
	return body, info, nil
}

type browseLFSBody struct {
	io.Reader
	bytesRead int
	closed    bool
}

func (r *browseLFSBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *browseLFSBody) Close() error {
	r.closed = true
	return nil
}
