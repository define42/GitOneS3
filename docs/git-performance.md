# Git transfer sizing and measurements

HTTP and SSH transfers stage incoming objects and deltas on disk, retain bounded
graph metadata in memory, and read object bodies individually. S3 stores immutable
packs indexed by the repository manifest. Existing loose objects remain readable.

## Resource bounds

| Resource | Bound per operation |
| --- | --- |
| Reachable decoded repository content | 1 GiB, at most 100,000 objects |
| One decoded object | 16 MiB |
| Serialized manifest | 64 MiB |
| Pack input or output | 1 GiB + 8 MiB |
| Incoming decoded workspace file contents | 2 GiB |
| Cached S3 packs on disk | 1 GiB + 8 MiB |
| Staged outgoing pack | 1 GiB + 8 MiB |

Allow approximately **4.1 GiB of temporary space per active push**, plus
filesystem overhead. HTTP fetch can use approximately 2.1 GiB for its pack cache
and staged response. Maintenance operations need additional space and do not
share the serving process's admission gate. The incoming workspace also enforces
a 1 GiB cumulative decoded-byte budget covering object bodies, delta instructions,
and external thin-pack bases; a delta-heavy push can reach that budget before
the final reachable repository reaches 1 GiB.

Set `TMPDIR` to disk-backed writable storage. Helm uses `/var/lib/gitone/work`.
Payload limits do not cap total process memory: graph maps, JSON decoding, zlib,
TLS, the S3 SDK, and Go's runtime also consume memory. Temporary files can add
filesystem page-cache usage to a container's memory accounting. Keep the default
of one active Git operation per shard until measurements in the target deployment
justify increasing it. See [admission settings](git-authentication.md#concurrency-and-memory).

The 90-second operation deadline remains in effect. Reaching the byte limit
depends on client and S3 throughput as well as CPU and disk performance.

## Native Git regression

Run the large transfer regression with native Git installed:

```sh
go test -tags=integration ./internal/gittransport \
  -run '^TestNativeGitLargeStreamingLifecycle$' -count=1 -v -timeout=5m
```

The test creates ten independent 8 MiB binary files in a separate native Git
process. It exercises:

- An 80 MiB Smart HTTP push, including Git's empty probe and chunked request.
- Full clones through HTTP and the SSH transport adapter.
- A thin-delta update of an 8 MiB file and an incremental HTTP fetch.
- Atomic branch/tag updates and deletions.
- Native `git fsck --full --strict` and repository integrity checks over the
  resulting 88 MiB of reachable history.

The object-store fixture saves payloads on disk. The SSH adapter runs over a local
stream without the SSH encryption/authentication layer; separate SSH server
integration tests exercise that layer.

An example local run sampled Go `HeapAlloc` every 2 ms: **0.5 MiB baseline,
43.5 MiB peak**, with approximately 848 MiB of object-store reads and 88 MiB of
writes across the complete lifecycle. These observations are informational, not
performance assertions. They exclude native Git client memory and do not measure
container RSS, filesystem page cache, real S3 latency, or concurrent traffic.
The regression verifies operation beyond the previous 64 MiB repository limit;
it does not establish capacity at every supported limit.

For deployment sizing, measure container memory, temporary disk usage, latency,
and the [storage metrics](storage-metrics.md) with representative repository
history and the intended concurrency. Include failed/cancelled pushes and
maintenance running alongside serving traffic.
