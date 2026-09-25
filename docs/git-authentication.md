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
| Single decoded Git object | 1 MiB |
| Complete reachable repository content | 64 MiB and 10,000 objects |
| Serialized object manifest | 1 MiB; can constrain object count below 10,000 |
| Published refs / updates per push | 1,000 each |
| Git HTTP request body | 72 MiB |
| Pack delta depth | 64 |
| Git request deadline | 90 seconds |
| Concurrent Git requests per shard handler | 1; busy requests receive `503` |

These are small-repository limits, not a production large-repository engine.
Pack inflation is also bounded; a highly compressed pack does not bypass
decoded-object limits. Reads load the bounded reachable object set, and fetch
does not optimize transfer by subtracting objects the client already has.
Ordinary Git clients can negotiate the supported protocol without special flags.
Tree entries are limited to 1,000 per directory, refs to 128-byte branch/tag
names, and commit/tag text to UTF-8; commit author/committer names are bounded
to 254 bytes. Unpublished objects from rejected or competing pushes can remain
in S3; automatic garbage collection is not implemented.
Shallow/partial clones, Git protocol v2, SHA-256 repositories, LFS, server hooks,
and branch-protection policy are not implemented. LFS routes return `501`.
Browser file editing and repository rename/delete operations remain unavailable.

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
