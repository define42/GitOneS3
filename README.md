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
- One HTTP listener for client and forwarded requests, internal-header stripping, and
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
- Optional Google or generic OIDC browser authentication with owner-shard code exchange,
  signed/encrypted Gorilla cookies, durable single-use login state, and atomic
  permanent username-to-provider-identity bindings.
- Shared user/group namespace claims, creator ownership, accepted invitations,
  group member roles, and conditional membership updates that preserve an owner.
- Paginated per-user shared-space discovery with authoritative membership checks
  and an explicit [existing-store migration](docs/space-discovery.md).
- Huma-backed, typed JSON APIs with generated OpenAPI and a GitHub-inspired
  React/TypeScript interface for registration, login, logout, and shared groups.
- Private repository creation and browsing in personal or shared namespaces,
  with optional README initialization, durable Git objects in S3, branch/file
  navigation, and commit history.
- Fine-grained, expiring personal access tokens and bounded native Git Smart
  HTTP clone, fetch, pull, and push, with owner-shard token verification and
  current namespace permissions.
- Optional Git-over-SSH clone/fetch/push, SSH public-key settings, authenticated
  shard forwarding, and live key/namespace permission checks. See
  [SSH setup](docs/git-ssh.md).
- Bounded live-compaction planning that keeps large packs intact, selects only
  fragmented small packs plus the incoming pack, and queues oversized work.
- S3-backed readiness, structured request logs, graceful HTTP
  shutdown, a non-root container image, and a configurable Helm deployment.
- Exact pack-fragmentation defaults from the architecture document.

## Deliberate Extension Points

The architecture document is a ten-phase platform plan and leaves several
external contracts unspecified. Git Smart HTTP is implemented for bounded
repositories; these parts remain extension points:

- the `control.git` compiler and persisted ACL generations;
- LFS batch/content/locking/quota handlers (currently `501 Not Implemented`);
- live pack compaction, retained-generation tracing, and garbage collection;
- production mTLS/workload-identity integration, rate limits, metrics, and
  audit sinks.

The code never falls back to a local bare repository for these operations.
Doing so would violate the S3-authoritative failure model.

## Project Layout

```text
cmd/gitone/                 process entry point
internal/app/               dependency wiring
internal/auth/              OIDC, callback routing, sessions, user/group bindings
internal/config/            environment and immutable cluster identity
internal/shard/             canonical paths, XXH64, owner calculation
internal/proxy/             one-hop streaming forwarding
internal/storage/           object and repository CAS contracts
internal/storage/s3store/   fixed-bucket AWS S3 adapter
internal/authz/             inherited shard-local authorization
internal/protocol/          Smart HTTP and LFS owner-side dispatch
internal/gittransport/      bounded Smart HTTP upload/receive-pack engine
internal/repository/        repository metadata, Git objects, and browser reads
internal/maintenance/       bounded compaction planning
internal/httpserver/        health, logging, and graceful lifecycle
internal/webui/             embedded UI assets and narrow browser routing
web/                       React/TypeScript application and Playwright tests
deploy/helm/gitone/         StatefulSet, Services, policy, identity
```

## Configuration

The process fails closed when required routing identity is absent or differs
from the mounted cluster identity.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GITONE_SPACE_DISCOVERY_MODE` | `indexed` | Shared-space discovery; use `scan` during the [two-phase migration](docs/space-discovery.md) |
| `GITONE_SHARD_COUNT` | required | Permanent routing modulus |
| `POD_NAME` | required | Canonical `gitone-N` owner ordinal |
| `POD_NAMESPACE` | required | Kubernetes namespace for stable DNS |
| `GITONE_LISTEN_ADDRESS` | `0.0.0.0` | Bind address |
| `GITONE_PUBLIC_PORT` | `8080` | Shared listener for clients and shard forwarding |
| `GITONE_INTERNAL_SCHEME` | `http` | Application-layer pod URL scheme; transport mTLS is transparent |
| `GITONE_HEADLESS_SERVICE` | `gitone-headless` | StatefulSet DNS Service |
| `GITONE_AUTH_ENABLED` | `false` | Enable OIDC authentication |
| `GITONE_PUBLIC_URL` | required when enabled | HTTPS origin without trailing slash |
| `GITONE_GOOGLE_CLIENT_ID` | required for Google | Google web OAuth client ID |
| `GITONE_GOOGLE_CLIENT_SECRET` | required for Google | Google OAuth client secret |
| `GITONE_OIDC_ISSUER` | unset | Generic HTTPS OIDC issuer, e.g. a Keycloak realm; use instead of Google credentials |
| `GITONE_OIDC_CLIENT_ID` | required with OIDC issuer | Generic OIDC client ID |
| `GITONE_OIDC_CLIENT_SECRET` | required with OIDC issuer | Generic OIDC client secret |
| `GITONE_COOKIE_HASH_KEY` | required when enabled | Base64 encoding of 64 random bytes, shared by all shards |
| `GITONE_COOKIE_BLOCK_KEY` | required when enabled | Base64 encoding of 32 random bytes, shared by all shards |
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

## Local Development

`make run` starts four GitOne instances, MinIO, Keycloak and a local HTTPS proxy
using Docker Compose. It creates the buckets and imports demo accounts
automatically. See [local setup, TLS trust and login instructions](deploy/compose/README.md).
Use `make stop` to stop the stack without deleting data, `make logs` for logs,
and `make smoke` to exercise real login and shared group access. With native Git
and Python on the host, `make smoke-git` validates real HTTPS Git operations.
The former single-process command is available as `make run-local`.

Open <https://gitone.localhost:8443> for the browser interface. Choose an available
GitOne username during registration, then authenticate with the configured OIDC
provider. Registration creates the permanent username binding; later sign-ins
use that same username and provider account. Shared groups have their own
available namespace and start with their creator as owner. Owners invite other
registered users by username, select their role, and manage accepted members.
Invitations grant access only after acceptance.

See [the browser interface and API guide](docs/browser-ui.md) for routes,
development builds, and browser tests. The UI is embedded in the Go binary and
uses the same listener as the API and shard forwarding; there is no second
application port or separately deployed frontend server.

## OIDC Providers

For Keycloak or another compatible provider, set `GITONE_OIDC_ISSUER`,
`GITONE_OIDC_CLIENT_ID`, and `GITONE_OIDC_CLIENT_SECRET`, together with enabled
auth, an HTTPS public URL, and shared cookie keys. Do not mix these with
`GITONE_GOOGLE_*` credentials. Register exactly
`<GITONE_PUBLIC_URL>/auth/oidc/callback` and start login at
`/<username>/auth/oidc/login`. The confidential client must support authorization
code flow, PKCE S256, RS256-signed ID tokens and the `openid email` scopes;
GitOne requires a nonempty verified email claim. Discovery pins the issuer;
TLS, issuer, audience, signature, expiry and nonce checks remain enabled.

Identities are the pair `(issuer, subject)`, never email or preferred username.
Non-Google IDs are `oidc:` followed by base64url SHA-256 of
`issuer + NUL + subject`. Existing Google records/sessions without an issuer
still mean Google, and their `google:<sub>` IDs remain unchanged. Provider
changes invalidate sessions/login attempts from the old issuer and do not
transfer username ownership or group membership. Account migration/linking is
not automatic. Configure the same provider on every shard and upgrade all
shards before enabling a non-Google provider.

## Google Login

Create a Google OAuth web client and register exactly
`https://git.example.com/auth/google/callback` as its authorized redirect URI.
Set the authentication variables above on every shard using the same client
credentials and cookie keys. Generate the keys independently with
`openssl rand -base64 64` and `openssl rand -base64 32`; keep them in your secret
manager. HTTPS is required at the public ingress; pod forwarding still uses
the single HTTP port. Incomplete enabled configuration fails startup.

Visit `/<username>/auth/google/login` to start login. The username follows the
existing lowercase namespace syntax; `auth` is reserved when authentication is
enabled. The first successful Google login claims that name. Subsequent logins
must have the same Google `sub`; matching email alone never grants ownership.
This is first-come registration, not a pre-provisioned account directory.
A Google account can currently bind more than one available name; there is no
global reverse subject-to-username index.

The existing username hash selects the owner shard (multiple users can share a
shard). Login starts there. Google always redirects to the fixed callback,
which may land on any pod. That pod verifies the Gorilla-protected state,
recomputes the owner from its username, and forwards to the owner's configured
DNS address. State cannot supply an arbitrary server URL. Only the owner
consumes the login transaction, exchanges the code, checks the ID token and
nonce, binds the username, and creates the session.

Login state expires after 10 minutes, is bound to a Secure/HttpOnly browser
cookie, and uses PKCE. Encrypted login records are stored under
`auth/transactions/` in the owner's bucket. A conditional write marks each
record used before code exchange, preventing concurrent replay across pod
restarts. Configure a lifecycle rule to remove records under that prefix after
one day, including old versions if bucket versioning is enabled. Permanent
User and group namespace records share `auth/users/<name>.json` for compatibility
with existing user bindings. Do not expire, delete, or reassign those records.

Successful login redirects to `/<username>` (also available as `/<username>/`),
which serves the browser interface for HTML navigation. JSON clients retain the
current identity and CSRF-token response, and
`GET /<username>/auth/session` returns that information explicitly.
Session cookies are signed, encrypted, Secure, HttpOnly, host-only, SameSite=Lax,
and expire after 12 hours. Shared keys allow verification after forwarding or
pod replacement. Changing keys invalidates existing sessions and login attempts;
coordinate key updates across shards.

With authentication enabled, browser namespace requests require a session. Personal
spaces are restricted to their bound account; groups check current membership
and the requested operation. The verified provider-scoped identity is passed to
downstream handlers and is returned as `userId` by the session endpoint.
Persisted per-repository ACL overrides remain an extension point; a session
does not grant access to another user's private space. Unsafe methods require
`Origin: <GITONE_PUBLIC_URL>` and `X-CSRF-Token: <csrfToken>`.
`POST /<username>/auth/logout` uses those protections and clears the browser
cookie; it does not revoke a copied cookie, which remains valid until expiry.
Liveness/readiness endpoints remain unauthenticated.

Native Git clients use a GitOne personal access token over HTTPS, not their
Google/Keycloak password or browser cookie. Manage tokens at `/auth/tokens`;
selected repositories are the recommended default. The optional all-accessible
scope includes current and future personal/group repositories, including groups
joined later, but always respects current namespace access and token permission.
See [Git authentication and transport limits](docs/git-authentication.md).
Git LFS remains unimplemented. Authentication is opt-in for existing deployments;
with it disabled, the previous unauthenticated protocol stubs remain.

Protocol references: [Google OIDC](https://developers.google.com/identity/openid-connect/openid-connect)
and [Gorilla securecookie](https://github.com/gorilla/securecookie).

## Shared Groups

An authenticated user creates a group with `POST /<group>` (or `/<group>/`).
No request body is required: the name comes from the URL, and the creator's
immutable user ID comes from their signed session. Send the session cookie,
`Origin`, and `X-CSRF-Token` as described above. A successful request returns
`201 Created`, the group record, and `Location: /<group>/`.

Users and groups share the same name space and naming/reserved-name rules.
The group name selects its owning shard using the existing hash, independently
of the creator's personal shard. A conditional S3 create makes concurrent user
registration and group creation mutually exclusive; taken names return `409`.
The common record retains the existing `auth/users/<name>.json` key so legacy
user bindings remain protected without a migration or a second claim index.

Groups have three roles, inherited by repositories below that group:

| Role | Access |
| --- | --- |
| `reader` | View the group and browse its repositories; Git fetch/LFS download permission |
| `developer` | Reader access plus repository creation; Git push/LFS mutation permission |
| `owner` | Developer access plus invitations and membership management |

Git Smart HTTP additionally requires the token's selected-repository or
all-accessible scope and read/write permission. Neither scope bypasses current
group membership or role. LFS still returns `501` after authorization.
Group roles apply across the whole namespace; per-repository overrides are
not implemented yet. Membership is read from S3 on each request rather than
embedded in the session, so revocations take effect on subsequent requests.

| Endpoint | Caller / purpose |
| --- | --- |
| `GET /<group>/` | Member: view group, roster, own role and CSRF token |
| `GET /<group>/members` | Owner: inspect membership |
| `GET /<group>/invitations` | Owner: inspect pending invitations |
| `POST /<group>/invitations` | Owner: invite or update an invitation with `{"userId":"google:SUB","role":"reader"}` |
| `POST /<group>/invitations/accept` | Invited user: accept using their own session; no body |
| `DELETE /<group>/invitations` | Owner: cancel with `{"userId":"google:SUB"}` |
| `PUT /<group>/members` | Owner: change an accepted member's role with `{"userId":"google:SUB","role":"developer"}` |
| `DELETE /<group>/members` | Owner: remove a member with `{"userId":"google:SUB"}` |

JSON bodies require `Content-Type: application/json`; all mutations require the
same session/Origin/CSRF checks. Members share their `userId` from their personal
`/<username>/auth/session` endpoint. Invitations target that immutable ID,
not an email or mutable display name, and grant access only after acceptance.
The UI resolves registered usernames to immutable IDs before inviting them and
provides invitation acceptance and membership management. There is no email
delivery. Pending invitations remain until accepted or canceled.

The creator starts as owner and may promote another accepted member to owner.
Removing or demoting the last owner returns `409`, including under concurrent
updates. Conditional-write conflicts are retried against current permissions;
persistent contention returns `409` so clients can retry. Groups are limited
to 1,000 members, 1,000 pending invitations, and a 128 KiB namespace record.
Renaming and deleting groups are not supported, and group names cannot be used
for Google login: members log in through their personal space.

## Repositories

Use **New repository** in the UI to create a repository in your personal space
or a shared group where you are a developer or owner. Repositories are private
and inherit their namespace's access rules: personal repositories are restricted
to the bound account, and group readers can browse but cannot create. There are
no public repositories or per-repository membership overrides in this UI.

Repository names are unique within their namespace and use 1–63 lowercase
letters, numbers, periods, underscores, or hyphens, with an alphanumeric first
and last character. Consecutive periods, the `.git` suffix, and the names
`auth`, `settings`, `members`, and `invitations` are reserved or invalid.
Creation claims the name atomically; a taken name returns a conflict.

Add an optional description. The UI uses `main` as the default branch; the API
also accepts a custom default branch. Enabling README initialization creates an
actual Git blob, tree, and initial commit in
the owning shard's S3 bucket, with durable refs and repository state. Without
initialization, the repository is empty. No local bare repository is used as
the source of truth.

Open `/<namespace>/<repository>` to browse branches, directories, file contents,
and commit history. File paths and refs are query parameters, not extra URL
path components. Browse requests enforce current namespace membership, so
removing a group member removes their subsequent repository access as well.

Clone from `https://<host>/<namespace>/<repository>.git`, using your GitOne
username and a personal access token covering that repository as the password.
Native Git push updates the same durable objects and refs that the browser reads.
An empty repository accepts its first push. See the
[Git guide](docs/git-authentication.md) for setup, group authorization, and
current size/protocol limits. Git LFS, browser file editing, repository deletion,
and renaming are not implemented.

## Build And Test

```sh
make build               # compile TypeScript and embed it in the Go binary
make test
make lint
make ui-check            # TypeScript checks and Vite production build
make test-ui             # Playwright against a running local Compose stack
go test -tags=integration ./internal/auth -run '^TestGitPAT'  # requires native Git
```

Local source builds require Go 1.26+ and Node.js 22.12+ with npm; `make run` builds both
inside Docker and needs neither on the host. Direct Go checks remain
`go test -race ./...` and `go vet ./...`. Run `make ui` before a direct
`go build ./cmd/gitone` to include the interface. A Go-only build from a fresh
checkout serves a clear UI-build-required page while leaving API/protocol
handlers available. Generated UI files are not committed.

[GitHub Actions CI](.github/workflows/ci.yml) runs on every push and pull request,
and can also be started manually. Separate jobs run Go unit tests with race
detection, randomized order, and coverage, and golangci-lint v2.13.2 using
`.golangci.yml`. Both jobs use the Go version from `go.mod`; neither requires
Node.js, Docker, or repository secrets. Browser and integration tests remain
separate from this Go unit-test workflow.

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
