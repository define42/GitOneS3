# Git LFS

GitOne supports the Git LFS basic transfer API: batch discovery, upload,
verification, and download, including single HTTP byte ranges. LFS is enabled by
default when authentication is configured. File locking and pure SSH content
transfer are not implemented; SSH remotes use SSH authentication followed by
HTTPS transfers.

## Client setup

Install Git LFS on your workstation and create the repository in GitOne first.
In your local repository:

```sh
git lfs install --local
git lfs track '*.bin'
git add .gitattributes asset.bin
git commit -m 'Track large assets with Git LFS'
git push origin main
```

The Git LFS pre-push hook uploads content before Git publishes its pointers.
Normal clone and pull download the content. `git lfs fsck` checks your local
files. The repository browser marks LFS files and shows their actual size.
Text files up to 1 MiB display their stored content. Binary and larger files
can be downloaded through GitOne using your browser session; storage URLs
are never exposed.

HTTPS remotes use your GitOne username and PAT, with the same repository scope
and read/write permissions as Git. Group readers can download; developers and
owners can upload. See [Git authentication](git-authentication.md).

SSH remotes use your registered SSH key. GitOne implements
`git-lfs-authenticate <namespace>/<repository>.git upload|download` and returns
the configured GitOne HTTPS URL with a scoped bearer credential. No PAT is
required for this flow. The credential is restricted to one repository and
operation and permits new requests for 15 minutes. Transfers admitted before
expiry may finish within their configured transfer timeout. Key registration
and namespace permissions are checked on every HTTP request and again before
upload publication, including across shards. Revoking a key or removing access
prevents subsequent authorization checks from succeeding. Protect the credential
like a PAT. See [SSH setup](git-ssh.md).

Batch actions advertise credential expiry so queued transfers can request fresh
actions before starting with expired credentials.

SSH upload responses omit the optional separate verification action: the PUT
already verifies the content, so a long successful upload does not require a
second request using an expired credential. PAT uploads also expose the standard
verification action.

## Shards and streaming

```text
Git LFS client <-> GitOne public endpoint <-> owning GitOne pod <-> private S3 bucket
```

The existing namespace hash chooses the owning shard. All LFS metadata and
content live in that shard's bucket under the repository ID. Every client URL
points to `GITONE_PUBLIC_URL`; GitOne never returns S3 URLs, redirects, or storage
credentials. An ingress pod forwards the request to the owner using the existing
shard routing.

Uploads use sequential S3 multipart parts with one reusable 8 MiB payload
buffer. GitOne calculates the SHA-256 OID and exact length while receiving data,
rechecks write authorization, and completes the multipart upload only after
validation. A verified metadata record makes the content discoverable. Each
upload has a unique physical key, so a failed or concurrent upload cannot
overwrite an existing verified object. Normal failures abort the multipart
upload; interrupted publication leaves recoverable storage for GC.

Downloads stream S3 response bodies through the pod with backpressure and
request cancellation. GET and HEAD return the OID as an ETag; GET supports
single ranges, `If-Range`, and `If-None-Match`. Interrupted streams close the
storage body and terminate the response. **LFS transfers do not use temporary
disk or buffer a complete file in memory.** Ordinary Git packs still use the
existing temporary workspace.

The buffer size is not a total process memory guarantee. Include SDK/TLS
buffers, metadata, simultaneous downloads, forwarded requests, and ordinary Git
operations when sizing pods. Admission limits apply to the owning process.
Configure ingress to stream request/response bodies, allow your LFS object size,
and permit transfers lasting the configured timeout.

## Configuration

| Environment | Helm value | Default |
| --- | --- | --- |
| `GITONE_LFS_ENABLED` | `lfs.enabled` | `true` |
| `GITONE_LFS_MAX_OBJECT_BYTES` | `lfs.maxObjectBytes` | `1GiB` |
| `GITONE_LFS_MAX_REPOSITORY_BYTES` | `lfs.maxRepositoryBytes` | `10GiB` |
| `GITONE_LFS_MAX_CONCURRENT_TRANSFERS` | `lfs.maxConcurrentTransfers` | `4` |
| `GITONE_LFS_MAX_QUEUED_TRANSFERS` | `lfs.maxQueuedTransfers` | `8` |
| `GITONE_LFS_QUEUE_TIMEOUT` | `lfs.queueTimeout` | `5s` |
| `GITONE_LFS_TRANSFER_TIMEOUT` | `lfs.transferTimeout` | `30m` |

Object limits may be raised up to 64 GiB; repository quotas up to 1 PiB and at
least one maximum-sized object. Active transfers must be 1–32, queued transfers
0–1024, queue timeout positive and at most 90 seconds, and transfer timeout
positive and at most 12 hours. Zero queued transfers means immediate rejection
when busy. Saturation returns HTTP 503. Batch/verify requests have a separate
bounded control capacity, a 30-second timeout, and a 1 MiB JSON limit; batches
contain at most 1,000 objects. LFS admission is separate from Git pack admission.
Each batch resolves repository identity once for its object lookups and checks
up to eight distinct objects concurrently. Repeated OIDs share a lookup; every
existing object still requires its verified record and payload size/version
check. Cancellation stops the batch's workers before releasing control capacity.

Disabling LFS disables its HTTP endpoints; it does not remove stored objects or
turn off Git pointer integrity enforcement or LFS maintenance. Deploy the same
limits and public origin on every shard. Compose accepts the same environment
overrides.

Quota is enforced under the durable repository lock. It includes completed
physical payloads, completed orphan payloads awaiting collection, and durable
reservations for unfinished uploads, without counting a completed payload twice.
Each repository is also limited to 100,000 physical objects and unfinished
reservations combined, including at most 1,024 outstanding reservations.
Completed history is recorded in durable quota counters, so ordinary uploads
perform a fixed number of storage operations instead of scanning older objects.
The reservation map is bounded independently of repository history.
Concurrent attempts for the same content ID reserve quota separately. Verified
publication selects one payload; any extra completed payload remains charged
until GC collects it. If an initialization write has an uncertain result before
streaming starts, the server attempts to abort that upload and release its
reservation under the lock. Failed cleanup remains charged or requires ledger
reconciliation before another upload can be admitted.
Quota excludes metadata, provider object versions, and Git pack storage. A
failed multipart abort retains its reservation for recovery. Reservations last
24 hours and remain charged until cleaned up. Quota exhaustion returns HTTP 507;
size limits return 413; hash/length mismatches return 422.

## Storage and recovery

The repository layout adds:

```text
repos/<repository-id>/lfs/objects/<random-id>       immutable raw bytes
repos/<repository-id>/lfs/verified/<sha256>.json    verified size and physical object version
repos/<repository-id>/lfs/uploads/<random-id>.json durable upload reservation
repos/<repository-id>/lfs/quota.json              physical quota counters and pending reservations
```

The quota ledger uses conditional writes under the repository lock. Before an
upload or collection changes its artifacts, it marks the ledger dirty. A dirty
ledger cannot admit another upload until a complete reconciliation has counted
physical payloads and outstanding reservations and validated verified records.
Failed writes, cancellation, and partial collection therefore preserve charges
or force reconciliation; they cannot silently release an orphan's quota.
Ordinary successful uploads do not need reconciliation.

**Upgrade all writers together.** Stop and drain every server and maintenance
process using the bucket before upgrading or rolling back. Older binaries do
not maintain the quota ledger and cannot safely run alongside this version.
Before starting an older version, remove `lfs/quota.json` from every repository
while all writers remain stopped. This ensures a later upgrade reconciles any
objects written by that older version instead of trusting stale counters.
An existing repository without a ledger is migrated under the lock on its first
new upload or applied GC. That one-time scan can be lengthy; run applied GC
during the maintenance window to initialize large repositories. Migration and
repair stop after 300,001 LFS artifacts and fail closed on corrupt records,
unsupported ledger schemas, storage failures, or more than 1,024 outstanding
reservations. GC can collect expired legacy reservations before rebuilding the
ledger. Keep this ledger with the rest of the repository during backup and
restore; do not edit counters or mutate LFS objects outside GitOne.

Grant the shard credentials multipart upload and abort permissions in addition
to existing read/list/write/delete permissions. On AWS, upload initiation, part
upload, and completion use `s3:PutObject`; abort uses `s3:AbortMultipartUpload`.
Encryption policies may require additional KMS permissions. See the
[AWS permissions reference](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-with-s3-policy-actions.html).

Configure an `AbortIncompleteMultipartUpload` lifecycle rule on `repos/`, for
example after two days. This covers a crash after S3 creates an upload but before
GitOne persists its upload ID. Use an interval longer than the maximum supported
transfer duration. This rule only aborts incomplete multipart uploads; completed
LFS objects must be retained for GitOne's reference-aware GC. See
[AWS multipart lifecycle configuration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html).

The local Compose MinIO release uses its server-wide stale-upload cleanup
settings instead; it does not accept that S3 lifecycle action. Compose configures
a 48-hour age threshold and six-hour cleanup interval. See the
[local stack guide](../deploy/compose/README.md).

Git pushes validate every reachable LFS pointer against verified content and
persist an LFS reference index in the generation manifest. Missing objects or
size mismatches reject the Git push. `repository check` hashes LFS content, and
restore verifies it before publishing. GC protects content referenced by every
retained generation, active reservations, and recently uploaded content. It
collects old unreferenced payloads and verified records, and aborts expired
reservations after the grace period. Retained history continues to consume quota.
See [repository maintenance](repository-maintenance.md) and
[multipart storage metrics](storage-metrics.md).

## Upgrade and migration

Stop traffic and replace **all** older readers and writers before accepting Git
pushes or repacks. New manifests contain an `lfs` reference index; older binaries,
including previous schema-2 readers, reject this field. A mixed fleet and rollback
after publication are unsupported. Preserve a storage backup before upgrading.

Existing repositories with externally hosted LFS pointers must upload the
corresponding content to GitOne. From a clone with all relevant Git refs, fetch
the old server's LFS content, then push it to GitOne before pushing Git refs:

```sh
git lfs fetch --all old-origin
git lfs push --all gitone
git push gitone --all
git push gitone --tags
```

Ensure local or committed LFS URL overrides do not keep directing uploads to the
old host. Maintenance also verifies LFS pointers in retained historical
generations; include content reachable only from those snapshots when migrating
an existing GitOne store. Missing historical content blocks collection and
restore until recovered.

Protocol reference: [Git LFS basic transfers](https://github.com/git-lfs/git-lfs/blob/main/docs/api/basic-transfers.md).
