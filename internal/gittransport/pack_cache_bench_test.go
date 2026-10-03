package gittransport

import (
	"io"
	"testing"
)

// BenchmarkPreparedPackClone measures an 8 MiB binary clone through production
// negotiation and response preparation. Authority records remain live reads;
// storage is in memory, so this measures local CPU/allocations, not S3 latency.
// Fixture creation and warming the immutable metadata caches are excluded.
func BenchmarkPreparedPackClone(b *testing.B) {
	for _, test := range []struct {
		name      string
		diskBytes int64
	}{
		{name: "uncached"},
		{name: "warm", diskBytes: 4 << 30},
	} {
		b.Run(test.name, func(b *testing.B) {
			fixture, _, head := benchmarkSnapshot(8)
			handler, objects, _ := preparedPackFixture(b, fixture, test.diskBytes)
			request := uploadRequest([]string{head}, nil, true, true)
			clone := func() {
				snapshot, err := handler.store.ReadGitReferences(b.Context(), "alice", "cached")
				if err != nil {
					b.Fatal(err)
				}
				response, err := handler.prepareUpload(b.Context(), snapshot, request)
				if err != nil {
					b.Fatal(err)
				}
				writeErr := response.write(b.Context(), io.Discard)
				closeErr := response.Close()
				if writeErr != nil || closeErr != nil {
					b.Fatalf("write=%v close=%v", writeErr, closeErr)
				}
			}
			clone()
			reads := objects.payloadReads.Load()
			b.SetBytes(8 << 20)
			b.ReportAllocs()
			for b.Loop() {
				clone()
			}
			b.ReportMetric(float64(objects.payloadReads.Load()-reads)/float64(b.N), "payload-reads/op")
		})
	}
}
