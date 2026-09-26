# Git authentication and Smart HTTP

GitOne supports native Git clone, fetch, pull, and push over HTTPS. Repositories
remain private and use the same S3-backed objects and refs as the browser UI.
Git Smart HTTP requires enabled Google or generic OIDC authentication; `make run`
configures this with Keycloak. GitOne still uses one application listener for
the UI, API, Git requests, and shard forwarding.

## Create a credential

1. Register or sign in through the browser using your GitOne username and the
   configured Google/Keycloak account.
2. For a selected-repository token, create a repository or join a shared group
   with access to an existing one. An all-accessible token can be created before
   any repositories exist.
3. Open **Access tokens** (`/auth/tokens`). Give the token a recognizable name,
   select specific repositories (the recommended default) or choose all
   accessible repositories, choose read or read/write permission, and set an
   expiration from 1 to 90 days. The default is 30 days.
4. Copy the token once and save it securely. GitOne stores a SHA-256 verifier,
   not the plaintext secret. Token listings cannot recover the secret.

Selected-repository tokens use exact `namespace/repository` scopes, not
wildcards. The optional all-accessible scope covers all current and future
personal and group repositories allowed by the account's current namespace
access rules. This is a broad grant: it also covers repositories in groups the
user joins later. Prefer selected repositories for least privilege.

`read` allows clone, fetch, and pull; `write` additionally allows push. Neither
scope bypasses identity checks, current namespace membership/role, token
permission, expiry, or revocation. Group readers cannot push even with a write
token, and removed members cannot fetch or push. Repository creation and
account/group/token management still require a browser session; an
all-accessible PAT does not authorize those APIs.

Each token record lives at `auth/tokens/<username>/<token-id>.json` in the
issuing user's shard bucket. It contains the SHA-256 verifier, immutable OIDC
issuer/subject binding, name, selected repositories or the all-accessible flag,
permission, creation/expiry timestamps, and revocation timestamp. The random
secret is never stored there.
Revocation conditionally updates that record; no separate token database or
shared internal token file is needed.

Token management is available through the Huma API and requires the matching
user's session, with `Origin` and `X-CSRF-Token` on mutations:

| Endpoint | Result |
| --- | --- |
| `POST /api/v1/users/{name}/tokens` | `201`, `{token, metadata}`; secret returned once |
| `GET /api/v1/users/{name}/tokens` | `{tokens: [...]}` containing metadata only |
| `DELETE /api/v1/users/{name}/tokens/{id}` | `204`; revoke the token |

Listing returns up to 1,000 records and an optional `nextCursor`. Pass that value
as `?after=<nextCursor>` for the next page; the UI exposes **Load more**.
Pagination bounds record reads, but the current object-store `List` still
materializes the user's complete token-key prefix. Large deployments need a
paged storage listing or token index.

Creation defaults to selected repositories when `allRepositories` is omitted or
`false`; `repositories` must then contain at least one exact repository scope:

```json
{
  "name": "Development laptop",
  "permission": "write",
  "repositories": ["alice/project", "acme/shared"],
  "expiresInDays": 30
}
```

To cover all accessible repositories, set `allRepositories` to `true` and omit
`repositories` (or supply an empty list). Combining this mode with a nonempty
repository list is rejected:

```json
{
  "name": "All accessible repositories",
  "permission": "read",
  "allRepositories": true,
  "expiresInDays": 30
}
```

Choose the shortest useful expiry and narrowest repository list. Signing out
of GitOne or the OIDC provider does not revoke PATs. Use **Revoke** if a token
is exposed or no longer needed; revocation cannot be undone.

## Use native Git

Copy the repository's HTTPS clone URL, without a token embedded in it:

```sh
git clone https://git.example.com/alice/project.git
cd project
git pull --ff-only
# Edit files and commit locally, then:
git push origin HEAD:main
```

When Git requests credentials, enter your **GitOne username** and use the
**personal access token as the password**. This is not your Google/Keycloak
password, email address, ID token, or browser session cookie. For a group
repository, still use your personal GitOne username, not the group name.

Use an OS-backed secure Git credential helper to avoid repeated prompts; see
[Git's credential documentation](https://git-scm.com/docs/gitcredentials).
When using different tokens for different repositories on the same host, enable
path-specific credential lookup in the cloned repository:

```sh
git config credential.useHttpPath true
```

Do not paste tokens into remote URLs, command-line arguments, source files,
shell history, or diagnostic logs. The UI never adds a token to the clone URL
or stores it in browser local/session storage. Copying a token puts it in the
system clipboard; clear it when finished. GitOne does not currently provide
an OAuth credential-helper/device-login flow. [SSH public-key authentication](git-ssh.md)
is also available when enabled by the operator.

For the local Compose certificate, follow the
[explicit CA trust instructions](../deploy/compose/README.md#local-https).
Do not set `GIT_SSL_NO_VERIFY` or `http.sslVerify=false`.

An empty repository can be cloned before its first commit. Add and commit a
file locally, then push `HEAD:main` (or its configured default branch). Pushed
files, branches, and commits become available to browser reads after durable
publication. Push does not create a missing GitOne repository; create it first.

## Shards, permissions, and revocation

The Git URL routes to the repository namespace's shard. The token's issuing
user namespace selects a separate authority shard when necessary. For a group
repository this is often a different shard:

```text
Git request -> repository owner shard -> user's token authority shard
                     |
            current namespace permissions
                     |
           authorized Git read or write
```

The repository shard verifies a PAT with its authority on each request; it does
not trust a caller-supplied identity header or a cached authorization result.
Verification uses the token itself and the configured shard resolver, not a
token-supplied server URL. It shares the existing listener and needs no second
port or internal token file. Protect internal traffic with the deployment's
network controls and transport TLS/workload identity where required.

The repository owner independently checks current namespace permissions and
the token's selected-repository or all-accessible scope. Before publishing a
push it rechecks the token and membership, then uses conditional S3 state
publication. Failed authorization or a competing state update must not publish
the attempted refs. Revocation blocks
subsequent authorization checks; it cannot recall data already downloaded or
make an authorization check atomic with a later membership change.

Missing, incorrect, expired, or revoked credentials return `401`; an accepted
token without the necessary scope/role returns `403`. An unavailable authority
fails closed with `503`, rather than accepting a stale positive result. Git
receive-pack can also report an individual push rejection in its protocol
response, so a successful HTTP status alone does not mean a push was accepted.

## Current transport limits

The implementation is a bounded Git protocol-v0 engine with Smart HTTP and SSH adapters, using SHA-1
Git objects. It validates incoming packs, object connectivity, and expected old
refs, then publishes all ref changes in one S3 compare-and-swap. No local bare
repository, Git subprocess, server hook, or local Git config is authoritative.

| Limit | Current bound |
| --- | --- |
| Single decoded Git object | 16 MiB (browser previews remain 1 MiB) |
| Complete reachable repository content | 1 GiB and 100,000 objects |
| Serialized object manifest | 64 MiB; graph metadata is bounded separately from payloads |
| Published refs / updates per push | 1,000 each |
| Pack input/output | 1 GiB + 8 MiB; push commands additionally bounded to 1 MiB |
| Incoming workspace file contents | 2 GiB; decoded bodies and delta instructions also share a 1 GiB budget |
| Upload-pack negotiation input (HTTP and SSH) | 1 MiB |
| Pack delta depth | 64 |
| Git request deadline | 90 seconds |
| Active Git operations per shard process | 1 by default; configurable from 1 to 32, shared with SSH |
| Waiting Git operations per shard process | 4 by default; configurable from 0 to 1,024 |
| Admission queue timeout | 5 seconds by default; positive duration up to 90 seconds |

HTTP and SSH use the same disk-backed pack decoder and canonical pack writer.
Incoming OFS/REF deltas, including thin-pack bases, are validated within byte,
object-count, depth, and disk budgets. Uploaded objects become immutable standard
Git packs with per-object offsets, CRC32, Git SHA-1, and SHA-256 in schema-2
manifests. Canonical packs contain independent zlib entries; stored delta chains
are not required for reads. Existing schema-1 loose objects remain readable.

Advertisements read bounded refs and manifest metadata. Fetch validates graph
connectivity, subtracts the client's common history, and encodes objects one at a
time. HTTP responses are staged in temporary files before response headers are
sent; SSH streams the encoded response. Object bodies and request/response packs
are processed without buffering the complete repository in memory. The small-repository `ReadGit` helper remains capped
at 64 MiB and is not used by network transfers.

Git pack caches, incoming workspaces, and outgoing pack files are disposable
local workspace. S3 remains authoritative. Set `TMPDIR` to writable storage;
Helm sets it to `/var/lib/gitone/work`. Allow approximately 4.1 GiB of temporary
file contents per active push plus filesystem overhead. Cancellation and normal
completion remove workspace files. After a process crash, remove leftover
`gitone-*` files/directories only while all users of that workspace are stopped.
Do not use a memory-backed workspace for large repositories.

Tree entries remain limited to 1,000 per directory, refs to 128-byte branch/tag
names, and commit/tag text to UTF-8. Commit author/committer names are bounded to
254 bytes. Concurrent writers use a durable repository lock plus the generation
CAS. A conflicting writer is rejected for retry. A crashed holder leaves a lock
requiring explicit operator recovery; reads remain available.

Rejected pushes may leave immutable artifacts. The [maintenance command](repository-maintenance.md)
collects old unreferenced artifacts with conditional deletes, retaining all
recoverable generations and validating them before deletion. It also checks
integrity, restores a chosen snapshot as a new generation, and repacks repositories.

Stop traffic and replace all older readers and writers before accepting pushes
with this release. Older binaries cannot read schema-2 pack manifests, so a
mixed-version rollout cannot serve newly published repositories. All writers
must also honor the durable lock before maintenance can run safely. Rollback
after the first packed push requires a compatible binary or restoring the
pre-upgrade storage backup. See the [upgrade instructions](repository-maintenance.md#prepare-the-deployment).

Git LFS supports larger files through streamed uploads/downloads with separate
limits, SHA-256 verification, quotas, and the same repository permissions.
See [Git LFS setup, storage, and upgrade requirements](git-lfs.md).

Shallow/partial clones, Git protocol v2, SHA-256 Git repositories, server hooks,
and branch-protection policy are not implemented. LFS file locking is unavailable.
Browser file editing and repository rename/delete operations remain unavailable.

## Concurrency and memory

One admission limit is shared by Smart HTTP and SSH Git operations on each shard
process, across all its repositories. The defaults keep one operation active
and permit four waiting operations for up to five seconds. A full queue or an
expired admission wait rejects the operation (`503` for HTTP, a Git/SSH error
for SSH). Cancellation or a connection/request deadline removes waiting work;
queued operations do not load repository objects while waiting. Setting the
queue capacity to zero restores immediate rejection when all active slots are
occupied. The queue timeout does not extend the request/connection deadline.

Configure these environment variables, or the matching Helm values:

| Environment | Helm value | Default | Allowed range |
| --- | --- | --- | --- |
| `GITONE_GIT_MAX_CONCURRENT_OPERATIONS` | `git.maxConcurrentOperations` | `1` | 1–32 |
| `GITONE_GIT_MAX_QUEUED_OPERATIONS` | `git.maxQueuedOperations` | `4` | 0–1024 |
| `GITONE_GIT_QUEUE_TIMEOUT` | `git.queueTimeout` | `5s` | Positive Go duration, at most `90s` |

Compose accepts the same environment overrides. These controls change runtime
admission only, not shard routing or repository/object limits. Concurrent pushes
still publish through repository compare-and-swap: increasing concurrency does
not make conflicting updates both succeed.

Before raising active concurrency, test overlapping clone/fetch/push operations
on your largest supported repositories inside the intended container limit.
Measure peak container memory, including the process baseline, repository
manifest/graph metadata, per-object decoding/encoding, and non-Go memory. Include
filesystem page cache and leave operational headroom. The repository byte limit
is not a memory-per-operation guarantee.
Queued connections also consume resources. `GOMEMLIMIT` is a soft Go runtime
memory target; it cannot enforce a hard process/container memory ceiling or
prevent an OOM kill. Keep the default of one until workload-specific measurements
justify a higher value. See [performance measurements](git-performance.md).

## Native-client integration tests

With Go and the native `git` executable installed, run:

```sh
go test -tags=integration -race ./internal/auth -run '^TestGitPAT' -count=1
```

The tests use two independent in-memory shard stores, real local HTTP forwarding,
and disposable working directories. They cover clone/push/pull, first push to
an empty repository, agreement with browser file/history APIs, token scopes,
read-only/revoked credentials, cross-shard group membership revocation, and an
unavailable token authority. Group coverage also checks live role downgrades
and token revocation at a different issuing shard. All-accessible token tests
create repositories/groups after issuance and check future access, expiry,
revocation, and denial of unrelated private spaces. The native Git helper reads ephemeral environment
credentials without putting tokens in remote URLs or command arguments; test
failure output redacts them. No Docker stack or external identity provider is
needed for these tests.

For a running `make run` stack, validate real HTTPS Git with Keycloak and MinIO:

```sh
make smoke-git
```

This requires Python 3.10+ and native Git on the host, verifies the generated
local CA, and resolves the two local HTTPS names to loopback without editing
the hosts file. It performs real OIDC login, personal and cross-shard group Git
operations, read-only/revoked-token checks, and browser-data comparisons.
Temporary working trees are removed and generated tokens are revoked on exit;
uniquely named user/group namespaces and repository data remain in MinIO.
