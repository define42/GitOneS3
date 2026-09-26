package repository

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
)

func TestWalkObjectsDeduplicatesRepeatedEdges(t *testing.T) {
	t.Parallel()
	objects := map[string]GitObject{}
	add := func(object GitObject) string {
		id := GitObjectID(object)
		objects[id] = object
		return id
	}
	blobID := add(GitObject{Type: "blob", Data: []byte("shared content")})
	leafID := add(GitObject{Type: "tree", Data: graphTreeEntry(t, "100644", "file", blobID)})
	var root []byte
	for branch := range 32 {
		var data []byte
		for alias := range 32 {
			// Each distinct branch references the same subtree many times.
			data = append(data, graphTreeEntry(t, "40000", fmt.Sprintf("alias-%02d-%02d", branch, alias), leafID)...)
		}
		branchID := add(GitObject{Type: "tree", Data: data})
		root = append(root, graphTreeEntry(t, "40000", fmt.Sprintf("branch-%02d", branch), branchID)...)
	}
	rootID := add(GitObject{Type: "tree", Data: root})
	refs := map[string]string{}
	for alias := range 128 {
		refs[fmt.Sprintf("refs/tags/alias-%03d", alias)] = rootID
	}
	infos, get, reads := graphFixture(objects)
	reachable, err := walkObjects(t.Context(), refs, infos, get)
	if err != nil {
		t.Fatal(err)
	}
	if len(reachable) != len(objects) {
		t.Fatalf("reachable objects=%d, want %d", len(reachable), len(objects))
	}
	for id, object := range objects {
		want := 1
		if object.Type == "blob" {
			want = 0 // Blob bodies have no edges and are checked during transfer.
		}
		if reads[id] != want {
			t.Errorf("%s %s loaded %d times, want %d", object.Type, id, reads[id], want)
		}
	}
}

func TestWalkObjectsRejectsConflictingTypesAfterDeduplication(t *testing.T) {
	t.Parallel()
	for _, visited := range []bool{false, true} {
		t.Run(fmt.Sprintf("target_already_visited=%v", visited), func(t *testing.T) {
			t.Parallel()
			leaf := GitObject{Type: "tree"}
			leafID := GitObjectID(leaf)
			objects := map[string]GitObject{leafID: leaf}
			var root []byte
			if !visited {
				// The first edge schedules the target as a tree. The second
				// requires that same target to be a blob before it is visited.
				root = append(root, graphTreeEntry(t, "40000", "a-correct", leafID)...)
				root = append(root, graphTreeEntry(t, "100644", "b-conflict", leafID)...)
			} else {
				branch := GitObject{Type: "tree", Data: graphTreeEntry(t, "100644", "conflict", leafID)}
				branchID := GitObjectID(branch)
				objects[branchID] = branch
				// The LIFO walk visits the valid target before the branch
				// that later requires the target to have a different type.
				root = append(root, graphTreeEntry(t, "40000", "a-branch", branchID)...)
				root = append(root, graphTreeEntry(t, "40000", "b-correct", leafID)...)
			}
			rootObject := GitObject{Type: "tree", Data: root}
			rootID := GitObjectID(rootObject)
			objects[rootID] = rootObject
			infos, get, reads := graphFixture(objects)
			_, err := walkObjects(t.Context(), map[string]string{"refs/tags/root": rootID}, infos, get)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("conflicting object type accepted: %v", err)
			}
			wantReads := 0
			if visited {
				wantReads = 1
			}
			if reads[leafID] != wantReads {
				t.Fatalf("target loads=%d, want %d to exercise the intended traversal state", reads[leafID], wantReads)
			}
		})
	}
}

func graphTreeEntry(t *testing.T, mode, name, id string) []byte {
	t.Helper()
	rawID, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(mode+" "+name+"\x00"), rawID...)
}

func graphFixture(objects map[string]GitObject) (map[string]objectInfo, gitGetter, map[string]int) {
	infos := make(map[string]objectInfo, len(objects))
	for id, object := range objects {
		infos[id] = gitObjectInfo(object)
	}
	reads := map[string]int{}
	get := func(ctx context.Context, id string) (GitObject, error) {
		if err := ctx.Err(); err != nil {
			return GitObject{}, err
		}
		object, ok := objects[id]
		if !ok {
			return GitObject{}, ErrNotFound
		}
		reads[id]++
		return object, nil
	}
	return infos, get, reads
}
