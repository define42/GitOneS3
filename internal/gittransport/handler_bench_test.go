package gittransport

import (
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
)

// BenchmarkHandlerUpload isolates negotiation, reachability and pack encoding.
// Fixture creation and ReadGit are deliberately excluded; the opt-in memory
// harness measures the complete HTTP/storage pipeline separately.
func BenchmarkHandlerUpload(b *testing.B) {
	for _, mode := range []string{"clone", "incremental", "up-to-date"} {
		b.Run(mode, func(b *testing.B) {
			snap, old, head := benchmarkSnapshot(8)
			body := pkt("want "+head+" side-band-64k\n") + "0000"
			if mode == "incremental" {
				body += pkt("have " + old + "\n")
			} else if mode == "up-to-date" {
				body += pkt("have " + head + "\n")
			}
			request := []byte(body + pkt("done\n"))
			handler := &Handler{}
			var responseBytes int
			b.ReportAllocs()
			for b.Loop() {
				response, err := handler.upload(b.Context(), snap, request)
				if err != nil {
					b.Fatal(err)
				}
				responseBytes = len(response)
			}
			b.ReportMetric(float64(responseBytes), "response-bytes/op")
		})
	}
}

func benchmarkSnapshot(megabytes int) (*repository.GitSnapshot, string, string) {
	objects := make(map[string]repository.GitObject)
	random := rand.NewChaCha8([32]byte{42})
	var tree []byte
	for index := range megabytes {
		data := make([]byte, 1<<20)
		_, _ = random.Read(data)
		object := repository.GitObject{Type: "blob", Data: data}
		id := repository.GitObjectID(object)
		objects[id] = object
		tree = append(tree, fmt.Sprintf("100644 file-%03d\x00", index)...)
		hash, _ := hex.DecodeString(id)
		tree = append(tree, hash...)
	}
	treeObject := repository.GitObject{Type: "tree", Data: tree}
	treeID := repository.GitObjectID(treeObject)
	objects[treeID] = treeObject
	commit := func(parent, message string) string {
		data := "tree " + treeID + "\n"
		if parent != "" {
			data += "parent " + parent + "\n"
		}
		data += "author Perf <perf@example.test> 1 +0000\ncommitter Perf <perf@example.test> 1 +0000\n\n" + message + "\n"
		object := repository.GitObject{Type: "commit", Data: []byte(data)}
		id := repository.GitObjectID(object)
		objects[id] = object
		return id
	}
	old := commit("", "initial")
	head := commit(old, "metadata-only incremental commit")
	return &repository.GitSnapshot{
		DefaultBranch: "main", References: map[string]string{"refs/heads/main": head}, Objects: objects,
	}, old, head
}
