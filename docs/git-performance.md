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
| Cached decoded commits, trees and tags | 4 MiB, at most 4,096 objects |

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

## Sparse and dense reads

Git readers fetch individual verified pack ranges for sparse requests. Reading
a commit from an old pack does not download the unrelated blobs in that pack.
The request's decoded metadata cache avoids repeated range reads during graph
validation and the walks of wanted and already-held objects. Its byte and object
limits apply independently; larger histories continue reading from storage.

When a planned read covers at least half a pack's bytes, the reader downloads
that pack once to its bounded temporary disk cache. Full clones, integrity
checks, and dense metadata or LFS-pointer scans use this path to avoid one S3
request per object. Thin pushes plan all candidate external bases after the
incoming pack checksum is verified; only published reachable objects can be
prefetched. This also coalesces dense base reads without making a one-base
update download unrelated blobs. Every decoded entry still verifies its packed CRC, Git
object ID and independent SHA-256. Downloaded full packs additionally verify the
SHA-256 in their storage key. Temporary operation caches are removed when the
operation closes; the shared serving cache below can retain verified content
for later requests.

## Shared serving cache

The serving process shares one bounded cache between Git HTTP, Git SSH, and
repository browser reads. RAM retains validated manifests, refs, decoded Git
metadata and generation-specific graph indexes. The optional disk tier retains
verified immutable snapshot bytes, packs, sparse pack entries, and outgoing
clone packs. Repeated reads can therefore avoid S3 downloads and repeated
parsing or graph reads.
An oversized item falls back to the existing bounded read path.

Clone requests with no client `have` history can reuse an outgoing pack keyed
by the exact generation, selected object IDs, and encoding version. HTTP and
SSH share the raw pack; negotiation and transport framing remain request-local.
HTTP streams a pinned cached file without making another temporary copy.
Incremental fetch responses are not retained. Building a clone pack reserves
space based on its selected decoded objects: twice their bytes plus 1 KiB per
object and 32 bytes, capped at the maximum pack size (1 GiB + 8 MiB). This cache
estimate avoids reserving a large pack's space for a tiny clone. If space is
unavailable or the encoded pack exceeds the estimate, the request uses the
existing temporary-file path. The protocol's pack limit still applies. Active
readers pin files against eviction. Source packs and outgoing packs share the
same disk budget.

| Setting | Direct binary / Compose default | Helm default |
| --- | --- | --- |
| `GITONE_CACHE_MEMORY_BYTES` | `256MiB` | `cache.memoryBytes: 256MiB` |
| `GITONE_CACHE_DISK_BYTES` | `0` | `cache.diskBytes: 16GiB` |
| `GITONE_CACHE_DIRECTORY` | unset | `/var/cache/gitone` |

Zero disables each tier independently. Byte values accept `B`, `KiB`, `MiB`,
`GiB`, `TiB`, and `PiB`, or a plain integer. Disk caching requires an absolute
writable directory. The application namespaces cached content by S3 endpoint,
region, and derived bucket, so reuse of a directory cannot mix different stores.
The disk tier supports Linux, macOS, DragonFly BSD, FreeBSD, NetBSD, OpenBSD,
and illumos. Other platforms can use the memory tier with disk caching disabled.
Keep the disk budget below the filesystem/volume capacity with room for cache
files and filesystem overhead. Helm's disk cache uses a 20 GiB `emptyDir`;
replacement pods start cold. The cache is disposable and S3 remains authoritative.

Memory accounting estimates retained cache values and entry overhead; it is not
a process RSS limit. Active readers can retain references after eviction, and decoding,
in-flight operations, Go's runtime, and filesystem page cache need additional
memory. Add cache budgets to the operation resource bounds above. Shared cache
storage and `TMPDIR` are separate budgets. Do not increase operation concurrency
solely because a cache is enabled.

The disk cache also caps its index at 100,000 entries to bound metadata for
small objects. Restart recovery scans directory entries in batches instead of
loading the entire directory listing into RAM. Entry-count pressure can evict
cached content even when the disk byte budget is not full.

### Freshness and integrity

Current repository-state pointers, repository identity, tokens, permissions,
and write preconditions continue to be read from their authority. Requests pin
one immutable generation and only reuse content validated for that generation.
Caching does not extend a revoked user's access to private repositories.

A verified cache hit can continue serving known-good immutable content even if
the corresponding S3 object is subsequently corrupted or removed outside
GitOne. Serving requests therefore may not immediately detect that backend
damage. Repository integrity, repack, restore, and collection operations bypass
the serving cache and inspect authoritative storage. Use those checks to verify
durability; set both cache budgets to zero when diagnosing uncached serving
behavior. Existing S3 data needs no migration.

### Qualify cold and warm workloads

Measure both empty-cache and warmed-cache runs with representative repositories,
namespace skew, and overlapping Git/browser traffic. Compare S3 request counts,
cache hit/miss and eviction counters, latency, CPU, container memory, and disk
usage. Include working sets larger than the configured budgets, cache loss,
permission revocation, and a newly published generation. Cache metrics are
appended to the existing authenticated `/system/metrics` endpoint. Cache budgets
and local benchmark results do not establish a cluster user-capacity guarantee.

### Local warm-cache measurements

Measured on 2026-10-03 with Go 1.27.1, Linux/amd64, Intel Core i7-7700HQ,
and one Go CPU. These are medians of six serial samples with a 300 ms benchmark
target; fixture creation and warmup are excluded. The object store is in memory.
Clone output includes sideband framing and is sent to `io.Discard`.

| Operation | Baseline time | Warm time | Baseline allocated bytes | Warm allocated bytes |
| --- | ---: | ---: | ---: | ---: |
| Prepare and send an 8 MiB binary clone | 142.25 ms | 6.79 ms | 50.66 MiB | 30.10 MiB |
| Load, validate and select a 1,000-commit history | 76.31 ms | 0.173 ms | 50.45 MiB | 25.74 KiB |

The clone comparison uses warm RAM metadata in both cases, with disk caching
disabled in the baseline and a warm 4 GiB disk cache in the second case. Payload
storage reads fall from one to zero per clone. The history comparison disables
both tiers in the baseline and uses a warm 64 MiB RAM cache with disk disabled.
Total storage reads fall from five to two; repository identity and current state
are still read on every operation. Both comparisons use a 64 MiB RAM budget
where RAM caching is enabled.

`benchstat` reports time reductions of 95.23% for the clone and 99.77% for the
history operation (`p=0.002`, six samples each). Allocated bytes are cumulative,
not peak memory. These local results exclude real S3, authentication, ingress,
network transfer and concurrent clients, so they do not establish production
throughput or support for a million active users. Reproduce the samples serially:

```sh
go test ./internal/gittransport -run '^$' -bench '^BenchmarkPreparedPackClone$' \
  -benchmem -count=6 -benchtime=300ms -cpu=1
go test ./internal/repository -run '^$' -bench '^BenchmarkCachedHistory$' \
  -benchmem -count=6 -benchtime=300ms -cpu=1
```

## Repository browser bounds

The repository page uses one `/browse` request for metadata, branches, and its
directory/README, file, or commit history. The server loads and validates one
published generation per page. Repository authority stays fresh, while verified
immutable manifest contents can be shared between requests by the serving cache.

Each shard admits at most four concurrent browser requests that load manifests.
This shared limit also covers the individual repository metadata, branches,
tree, blob, and commits endpoints. There is no browser queue: additional requests
receive `503` with `Retry-After: 1`. The existing 15-second API timeout applies,
and cancelled or failed requests release their slots. Catalog pagination, Git
transfers, and LFS transfers use their separate limits.

## Pack writer allocation measurement

Pack output reuses one compressor within each operation, resetting it after
closing every object's independent zlib stream. Buffers are never shared across
requests. A regression compares the resulting pack byte for byte with fresh
compressors, including each object's range, CRC and checksums.

A synthetic benchmark writes 1,000 small objects (24,000 decoded bytes) to
`io.Discard`, with input preparation excluded. On Linux/amd64, Go 1.27.1,
Intel Core i7-7700HQ at 2.80 GHz, six single-iteration samples with one Go CPU
gave these median allocations:

| Per 1,000-object pack | Fresh compressor per object | Reused compressor |
| --- | ---: | ---: |
| Total allocated bytes | 1,026.822 MiB | 1.685 MiB |
| Allocation count | 29,988 | 14,023 |

Allocated bytes fell 99.84%. These are cumulative allocations, not peak heap or
RSS, and this synthetic result does not measure Git transfer or S3 throughput.
The command used for each implementation was:

```sh
go test ./internal/gitpack -run '^$' -bench '^BenchmarkWriteSmallObjects$' \
  -benchmem -benchtime=1x -count=6 -cpu=1
```

## Local transfer comparison

Measured on 2026-10-02 against `91b4beb`, using Go 1.26.8, Linux/amd64,
Intel Core i7-7700HQ, `GOMAXPROCS=4`, a 63 MiB binary fixture, a disk-backed
object store and loopback HTTP. Values are medians of three serial samples.
For four clients, time covers completion of the whole concurrent batch and the
active Git limit is explicitly raised to four. This shared workstation experiment
does not include real S3, ingress, authentication or container page-cache usage.

| Operation | Clients | Before | After |
| --- | ---: | ---: | ---: |
| Full clone | 1 | 2.880 s | 3.085 s |
| Incremental fetch | 1 | 0.230 s | 0.0047 s |
| Full push | 1 | 3.493 s | 3.519 s |
| Thin-delta push | 1 | 4.442 s | 4.341 s |
| Full clone | 4 | 4.446 s | 4.685 s |
| Incremental fetch | 4 | 0.299 s | 0.0089 s |
| Full push | 4 | 5.650 s | 5.719 s |
| Thin-delta push | 4 | 6.381 s | 6.776 s |

The deterministic improvement is storage amplification: the incremental fetch
still returns 279 bytes, but reads **21,933 bytes instead of 66,103,358 bytes**
from storage, including repository metadata (99.97% less). Full transfers have
similar local durations; this experiment does not establish a throughput gain
for clones or pushes. The final dense thin-delta case performs one full base-pack
GET, avoiding one sequential S3 request per base blob.

Maximum observed process RSS across these samples was 23.7 MiB for one client
and 43.4 MiB for four. This includes the small HTTP clients and test harness,
excludes fixture creation in its separate process, and does not measure page
cache or maximum-size repository memory. The harness can be reproduced with:

```sh
GOTOOLCHAIN=go1.26.8 GOMAXPROCS=4 GITONE_PROFILE_MEMORY=1 \
GITONE_PROFILE_CLIENTS=1 GITONE_PROFILE_SCENARIOS=clone,incremental,push,delta-push \
go test -tags=integration ./internal/gittransport \
  -run '^TestHandlerPeakMemory$' -count=1 -v -timeout=15m
```

Repeat three times, then with `GITONE_PROFILE_CLIENTS=4`. Retain the
`GITONE_MEMORY` JSON records for comparisons on the same environment.

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

## Qualification near the supported limits

The opt-in capacity test streams fixture data into a separate native Git process
and uses the production HTTP handler with a disk-backed object store. It pushes,
clones, runs native `git fsck`, verifies exact object counts, and checks repository
integrity. `bytes` creates 1023 MiB of binary blobs; `objects` creates exactly
100,000 reachable objects, including directory trees and a commit.

```sh
GITONE_QUALIFY_CAPACITY=both go test -tags=integration ./internal/gittransport \
  -run '^TestNativeGitCapacity$' -count=1 -v -timeout=20m
```

Use `bytes` or `objects` to run one case. Run without race instrumentation, on
deployment-sized CPU, memory and disk. The test retains the production
90-second transfer deadlines, so an underprovisioned environment can fail even
though its repository is below the byte and object ceilings. JSON log records
include operation durations and storage reads/writes; the disk backend does not
qualify real S3 latency or request capacity. A small fixture smoke test runs in
the regular integration suite.

### Local S3 qualification result

The final code also passed `make qualify-s3` on 2026-10-02 using the same
Go 1.26.8 workstation, `GOMAXPROCS=4`, and the repository's conditional-delete
MinIO fixture in a local Docker container. Four concurrent 63 MiB clients with
four active Git slots completed four pushes and three rounds of clone/fetch:

| Request | Samples | Median | Maximum |
| --- | ---: | ---: | ---: |
| Push | 4 | 5.676 s | 5.750 s |
| Clone | 12 | 4.622 s | 4.955 s |
| Incremental fetch | 12 | 0.029 s | 0.031 s |

The same run passed restart/state persistence, crash-lock fencing, offline
unlock and retained-generation restore checks. The optional native capacity
cases then ran serially through S3, with the normal 90-second deadlines:

| Fixture | Push | Clone | Native fsck | Repository integrity |
| --- | ---: | ---: | ---: | ---: |
| 1023 MiB of blobs, 67 total objects | 69.109 s | 66.670 s | 5.332 s | 16.910 s |
| 100,000 total objects | 21.225 s | 13.392 s | 0.314 s | 5.677 s |

Each capacity row is one run and excludes fixture creation. These results
exercise the S3 adapter but do not establish capacity on a remote provider or
within a deployment's container memory/CPU limits. Follow the
[deployment qualification procedure](production-qualification.md) with the
intended resources, history shape, concurrency and storage service.
