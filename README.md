# GitOne S3

GitOne is a deterministic, namespace-sharded Git service foundation based on
the architecture in [GitOne_Configurable_Shard_S3_Architecture.pdf](GitOne_Configurable_Shard_S3_Architecture.pdf).
Each top-level namespace is permanently assigned with:

```text
xxhash64(canonicalTopLevelName) % shardCount
```

Every StatefulSet pod can receive a public request. A request is either served
by the local owner or streamed in one internal hop to the owning pod. Each pod
is permanently scoped to one S3 bucket; local storage is cache/workspace only.

```text
Git/LFS client -> public Service -> any gitone-N
                                      |
                              canonical path + XXH64
                                      |
                         local owner or one streamed hop
                                      |
                         fixed bucket for owner shard N
```

## Implemented

- Pure-Go XXH64 seed-0 routing with permanent test vectors and shard counts
  above 256.
- Strict canonical URL parsing that rejects case ambiguity, Unicode top-level
  keys, traversal, malformed escaping, encoded separators, and reserved names.
- Streaming `httputil.ReverseProxy` forwarding to stable StatefulSet DNS with
  context cancellation and no request/response body buffering.
- Separate public and internal listeners, internal-header stripping, and
  one-hop routing mismatch rejection.
- Immutable cluster identity validation for routing, path policy, and the
  per-shard S3 bucket mapping.
- Fixed-bucket AWS SDK v2 adapter for S3-compatible storage with conditional
  writes, validated range reads, listing, provider error translation, and a
  startup probe that rejects stores which ignore conditional operations.
- S3-native repository state publication: immutable artifacts and generation
  snapshots are durable before the mutable state pointer is updated by ETag
  compare-and-swap.
- Concurrent-CAS, immutable-object, routing, hostile-path, streaming,
  cancellation, configuration, ACL inheritance, and HTTP health tests.
- Private-by-default authorization primitives with public/internal visibility,
  immutable user IDs, inherited group grants, and direct repository grants.
- Bounded live-compaction planning that keeps large packs intact, selects only
  fragmented small packs plus the incoming pack, and queues oversized work.
- S3-backed readiness, structured request logs, graceful dual-listener
  shutdown, a non-root container image, and a configurable Helm deployment.
- Exact pack-fragmentation defaults from the architecture document.

## Deliberate Extension Points

The architecture document is a ten-phase platform plan and leaves several
external contracts unspecified. The owner-side dispatcher recognizes standard
Git Smart HTTP and Git LFS routes, but currently returns `501 Not Implemented`
until these engines are installed:

- namespace allocation and the globally consistent login/group registry;
- repository path metadata and creation APIs;
- `git-upload-pack` / `git-receive-pack`, pack validation, and ref semantics;
- the `control.git` compiler and persisted ACL generations;
- LFS batch/content/locking/quota handlers;
- live pack compaction, retained-generation tracing, and garbage collection;
- production mTLS/workload-identity integration, rate limits, metrics, and
  audit sinks.

The code never falls back to a local bare repository for these operations.
Doing so would violate the S3-authoritative failure model.

## Project Layout

```text
cmd/gitone/                 process entry point
internal/app/               dependency wiring
internal/config/            environment and immutable cluster identity
internal/shard/             canonical paths, XXH64, owner calculation
internal/proxy/             one-hop streaming forwarding
internal/storage/           object and repository CAS contracts
internal/storage/s3store/   fixed-bucket AWS S3 adapter
internal/authz/             inherited shard-local authorization
internal/protocol/          Smart HTTP and LFS owner-side dispatch
internal/maintenance/       bounded compaction planning
internal/httpserver/        health, logging, and graceful lifecycle
deploy/helm/gitone/         StatefulSet, Services, policy, identity
```

## Configuration

The process fails closed when required routing identity is absent or differs
from the mounted cluster identity.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GITONE_SHARD_COUNT` | required | Permanent routing modulus |
| `POD_NAME` | required | Canonical `gitone-N` owner ordinal |
| `POD_NAMESPACE` | required | Kubernetes namespace for stable DNS |
| `GITONE_LISTEN_ADDRESS` | `0.0.0.0` | Bind address |
| `GITONE_PUBLIC_PORT` | `8080` | Public Service listener |
| `GITONE_INTERNAL_PORT` | `8081` | Pod-to-pod listener |
| `GITONE_INTERNAL_SCHEME` | `http` | Application-layer pod URL scheme; transport mTLS is transparent |
| `GITONE_HEADLESS_SERVICE` | `gitone-headless` | StatefulSet DNS Service |
| `GITONE_CLUSTER_IDENTITY_FILE` | `/etc/gitone/identity/cluster-identity.json` | Immutable identity mount |
| `GITONE_S3_ENDPOINT` | AWS regional endpoint | S3-compatible endpoint |
| `GITONE_S3_REGION` | `us-east-1` | AWS region |
| `GITONE_S3_PATH_STYLE` | `false` | Path-style S3 addressing |
| `GITONE_S3_TLS` | `true` | Endpoint validation policy |
| `GITONE_S3_BUCKET_PREFIX` | `gitone-shard` | Per-shard bucket prefix |
| `GITONE_PATH_MAX_TOP_LEVEL_LENGTH` | `63` | Maximum permanent namespace-key length |
| `GITONE_PATH_MAX_COMPONENT_LENGTH` | `255` | Maximum decoded path-component length |
| `GITONE_PATH_MAX_DEPTH` | `32` | Maximum namespace/repository path depth |
| `GITONE_PACK_MAX_COUNT` | `32` | Pack-count compaction threshold |
| `GITONE_PACK_MAX_SMALL_COUNT` | `16` | Small-pack compaction threshold |
| `GITONE_PACK_SMALL_THRESHOLD` | `128MiB` | Small-pack size boundary |
| `GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_BYTES` | `2GiB` | Synchronous compaction byte bound |
| `GITONE_PACK_LIVE_COMPACTION_MAX_INPUT_PACKS` | `32` | Synchronous compaction pack bound |

AWS credentials use the SDK's normal provider chain. The buckets must exist
before pods start. Each process checks that its bucket honors `If-Match` and
`If-None-Match` writes before it starts serving; failed
probes keep the shard unavailable. Production qualification must also run
concurrent CAS acceptance tests against the exact provider/version. Configure a
lifecycle rule for abandoned objects and old versions below
`maintenance/capabilities/` when bucket versioning is enabled.

## Build And Test

```sh
make build
make test
make lint
```

Direct equivalents are `go test -race ./...`, `go vet ./...`, and
`go build ./cmd/gitone`.

## Kubernetes

The Helm chart derives the StatefulSet replica count directly from
`shardCount` and writes the same value into an immutable ConfigMap:

```sh
helm install gitone deploy/helm/gitone \
  --set shardCount=256 \
  --set image.repository=registry.example/gitone \
  --set image.tag=VERSION \
  --set s3.bucketPrefix=globally-unique-gitone-prod-shard
```

Shard forwarding requires no shared token or caller authentication. Requests
are forwarded directly to their owner with a one-hop marker. Transport mTLS
can be provided independently by a service mesh.

The base chart uses one ServiceAccount for the StatefulSet. Application code is
fixed to the ordinal-derived bucket, but a shared ServiceAccount is not
per-shard credential isolation. Deployments that require bucket-scoped IAM must
use an ordinal-aware admission/credential broker (the chart exposes
`podAnnotations`) or a shard-specific workload topology.

The immutable identity ConfigMap is annotated with Helm's `keep` policy so an
uninstall does not erase the routing and storage mapping. Back it up and
reinstall with the same release name, namespace, and identity values. Never
delete or regenerate it after allocating namespaces without a data migration.

`shardCount`, the XXH64 variant, canonicalization/path limits, S3 mapping, and
top-level names are migration boundaries. They must not change after the first
namespace is allocated without an explicit data migration.
