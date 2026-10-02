package gitpack

import (
	"fmt"
	"io"
	"testing"
)

// Thousands of small objects isolate compressor setup from payload processing.
func BenchmarkWriteSmallObjects(b *testing.B) {
	objects := make(map[string]Object, 1000)
	ids := make([]string, 0, 1000)
	var size int64
	for i := range 1000 {
		object := Object{Type: "blob", Data: []byte(fmt.Sprintf("small Git object %06d\n", i))}
		id := objectEntry(object).ID
		objects[id] = object
		ids = append(ids, id)
		size += int64(len(object.Data))
	}
	resolve := objectResolver(objects)
	b.ReportAllocs()
	b.SetBytes(size)
	for b.Loop() {
		if _, err := Write(b.Context(), io.Discard, ids, resolve, testLimits()); err != nil {
			b.Fatal(err)
		}
	}
}
