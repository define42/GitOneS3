package repository

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/define42/GitOneS3/internal/storage"
)

// BenchmarkCachedHistory compares the production snapshot/validation/reachability
// path for 1,000 commits. The object store is in memory; timings exclude fixture
// creation and warmup and do not represent remote S3 or deployment capacity.
func BenchmarkCachedHistory(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "uncached"
		if cached {
			name = "warm"
		}
		b.Run(name, func(b *testing.B) {
			objects := &cacheReadStore{ObjectStore: storage.NewMemoryStore()}
			store, err := New(objects)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := store.Create(b.Context(), "alice", createInput("history", false)); err != nil {
				b.Fatal(err)
			}
			base, err := store.ReadGitReferences(b.Context(), "alice", "history")
			if err != nil {
				b.Fatal(err)
			}
			blob := GitObject{Type: "blob", Data: []byte("history fixture\n")}
			blobID := GitObjectID(blob)
			raw, err := hex.DecodeString(blobID)
			if err != nil {
				b.Fatal(err)
			}
			tree := GitObject{Type: "tree", Data: append([]byte("100644 file.txt\x00"), raw...)}
			treeID := GitObjectID(tree)
			gitObjects := map[string]GitObject{blobID: blob, treeID: tree}
			var head string
			for i := range 1000 {
				parent := ""
				if head != "" {
					parent = "parent " + head + "\n"
				}
				commit := GitObject{Type: "commit", Data: []byte(fmt.Sprintf("tree %s\n%sauthor Test <test@example.test> 1 +0000\ncommitter Test <test@example.test> 1 +0000\n\nCommit %d\n", treeID, parent, i))}
				head = GitObjectID(commit)
				gitObjects[head] = commit
			}
			if err := store.PublishGit(b.Context(), base, []RefUpdate{{Name: "refs/heads/main", New: head}}, gitObjects, func(context.Context) error { return nil }); err != nil {
				b.Fatal(err)
			}
			if cached {
				store.cache = repositoryTestCache(b, 64<<20, 0)
			}
			read := func() {
				snap, err := store.ReadGitReferences(b.Context(), "alice", "history")
				if err != nil {
					b.Fatal(err)
				}
				reader, err := store.OpenGit(b.Context(), snap)
				if err != nil {
					b.Fatal(err)
				}
				if err := reader.Validate(b.Context()); err != nil {
					b.Fatal(err)
				}
				ids, err := reader.Reachable(b.Context(), snap.References)
				if err != nil || len(ids) != 1002 {
					b.Fatalf("objects=%d error=%v", len(ids), err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}
			read()
			objects.reset()
			b.ReportAllocs()
			for b.Loop() {
				read()
			}
			b.ReportMetric(float64(len(objects.reads()))/float64(b.N), "storage-reads/op")
		})
	}
}
