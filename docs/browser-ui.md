# Browser interface and API

GitOne's interface is a React/TypeScript application with a GitHub-inspired
header, personal and group spaces, repository browsing, accessible forms, and owner settings. Vite
builds static assets that Go embeds into the GitOne executable. Browser pages,
JSON APIs, authentication callbacks, Git/LFS requests, and shard forwarding all
use the existing listener.

Authentication must be enabled and configured for the session/group/repository API and UI
flows (`GITONE_AUTH_ENABLED=true` with provider credentials, public HTTPS URL,
and shared cookie keys). `make run` supplies this configuration. An installation
with authentication disabled keeps the existing protocol stubs but does not
provide the UI's Huma session/group/repository API.

## User flows

1. Open `/auth/register`, choose an available lowercase username, and authenticate
   with the configured Google or generic OIDC provider. The verified provider
   identity permanently claims that username; an email match alone is not enough.
2. For later sessions, open `/auth/login` and use the registered username with
   the same provider account. The UI reads its session from the authenticated
   API; the browser never receives the signed cookie's encryption keys.
3. Create a shared group from `/auth/new-group`. Users and groups compete for
   the same namespace. Creation is conditional, so an availability check does
   not reserve a name; concurrent claims can still return a conflict.
4. An owner invites a registered user by username and chooses reader, developer,
   or owner. The UI resolves the username to a provider-scoped immutable ID.
   Invitees accept before receiving access. Accepted members inherit their role
   throughout the group, and owners can cancel invitations or change membership.
5. Use **New repository** to create a private repository in your own namespace
   or a group where you are a developer or owner. Optionally initialize a README
   and then browse branches, directories, files, and commit history. Group
   readers can browse existing repositories but cannot create them.
6. Sign out to clear the GitOne session. The identity provider has its own SSO
   session; signing out of GitOne does not revoke a copied cookie or end that
   external session. A copied GitOne cookie remains valid until its expiry.

Personal spaces live at `/<username>`; shared spaces at `/<group>`. Group
settings use `/<group>/settings`, and direct invitation links use
`/<group>/invitations/accept`. Create repositories at
`/auth/new-repository?namespace=<namespace>` and browse them at
`/<namespace>/<repository>`, optionally with `ref`, `path`, or `view=commits`
query parameters. These routes return the same public HTML shell;
private data and mutations are always checked by the API on the owning shard.
An unavailable or forbidden API response must not be treated as access merely
because the browser can load the shell.

Repository creation and browsing use durable repository metadata and actual Git
objects in S3. README initialization writes a blob, tree, and initial commit;
an uninitialized repository has no commits. The UI uses `main` as the default
branch; the API can select another valid branch. Names are unique within their namespace, and
conditional creation prevents overwriting an existing repository.

All repositories are private and inherit namespace authorization. Personal
repositories belong to the bound user; group membership is checked on each API
request. Developers and owners may create group repositories; readers may
browse. Public visibility and per-repository ACL overrides are not exposed.

Git Smart HTTP and LFS remain extension points and return `501`; repository
creation does not enable Git client clone, push, or pull. Repository editing,
renaming, and deletion are not implemented.

Browsing currently selects branch names (not tags or arbitrary commit IDs).
History returns at most 100 first-parent commits, directories at most 1,000
entries, and file previews are bounded to 1 MiB. Binary files are identified
without rendering their contents. Namespace listings support up to 1,000
repositories; pagination is not implemented. Login return links are bounded
to 1,024 serialized bytes so signed OIDC state stays within its encoding limit.

## Huma API

The typed HTTP API is defined with [Huma](https://huma.rocks/) under `/api/v1/`.
Generated documentation is at `/api/docs`, with the OpenAPI schema at
`/api/openapi.json`. The original namespace-scoped JSON endpoints remain
available for existing clients; browser navigation is selected with
`Accept: text/html`.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/v1/session` | Session status, provider, shard count, and signed-in identity/CSRF token |
| `POST /api/v1/logout` | Clear the GitOne session cookie |
| `GET /api/v1/names/{name}` | Check namespace availability without reserving it |
| `GET /api/v1/users/{name}` | Resolve a registered username to an immutable user ID (signed-in callers) |
| `GET /api/v1/spaces?shard=N` | List the caller's memberships and invitations on one shard |
| `GET /api/v1/groups/{name}` | Read an authorized group view |
| `POST /api/v1/groups/{name}` | Claim a group and make the caller owner |
| `GET /api/v1/groups/{name}/invitation` | Read the caller's pending invitation |
| `POST /api/v1/groups/{name}/invitations` | Invite a user with `{userId, role}` |
| `DELETE /api/v1/groups/{name}/invitations` | Cancel an invitation with `{userId}` |
| `POST /api/v1/groups/{name}/invitations/accept` | Accept the current user's invitation |
| `PUT /api/v1/groups/{name}/members` | Change a role with `{userId, role}` |
| `DELETE /api/v1/groups/{name}/members` | Remove a member with `{userId}` |
| `GET /api/v1/repos/{namespace}` | List accessible repositories in a personal or group namespace |
| `POST /api/v1/repos/{namespace}` | Create a private repository; requires namespace write access |
| `GET /api/v1/repos/{namespace}/{repository}` | Read repository metadata and the caller's access |
| `GET /api/v1/repos/{namespace}/{repository}/branches` | List published branches |
| `GET /api/v1/repos/{namespace}/{repository}/tree?ref=main&path=docs` | Browse a directory on a branch |
| `GET /api/v1/repos/{namespace}/{repository}/blob?ref=main&path=README.md` | Read a file on a branch |
| `GET /api/v1/repos/{namespace}/{repository}/commits?ref=main` | Read branch commit history |

Huma validates request bodies and documents their schemas and errors. Signed
session cookies authenticate requests; mutations also require the configured
public `Origin` and the session's `X-CSRF-Token`. Group membership is read from
the authoritative shard's S3 bucket and checked again during conditional
updates. The UI is not an authorization boundary.

Space discovery currently lists namespace keys on each shard and then reads
records with a 10,000-record cap and a 15-second request deadline. The storage
`List` operation materializes the entire key prefix before that cap is applied;
the cap does not bound listing memory. The UI combines each shard's results.
Large deployments need a membership index before relying on this dashboard at
scale.

## Build and development

```sh
make run             # complete Docker stack, including frontend build
make build           # host Node/npm + Go build, embeds the UI in bin/gitone
make ui              # install locked dependencies, check TypeScript, build UI
make ui-check        # TypeScript and production build without copying Go assets
make run-local       # build UI, then run Go using your configured environment
```

Source lives in `web/`; build output lives in `web/dist/` and is copied to
`internal/webui/dist/` for embedding. Generated assets and dependencies are
ignored by Git. Docker uses a separate Node build stage, so runtime images need
neither Node nor a frontend server. A clean checkout can still run `go test
./...` without npm: Go embeds a small build instruction marker until `make ui`
or a Docker build supplies the real interface.

The server serves only named HTML navigation routes and `/gitone/assets/*`.
Repository pages require a canonical two-component namespace/repository path
and an explicit `Accept: text/html`; a trailing slash is supported. API,
callback, `.git`, Git protocol, LFS, and deeper repository paths are not rewritten
into HTML. The HTML shell is `no-store`; content-hashed Vite assets are immutable.
The UI uses same-origin scripts/styles and a restrictive Content Security
Policy without inline scripts or external CDNs.

## Verification

Unit tests cover static-route separation, security headers, no-store HTML,
immutable assets, HEAD requests, missing builds, repository-name validation,
Git protocol passthrough, and traversal rejection:

```sh
go test -race ./internal/webui
make ui-check
```

After `make run`, install the browser test tools and run the end-to-end suite:

```sh
npm --prefix web ci
cd web
npx playwright install chromium
cd ..
make test-ui
```

Tests target `https://gitone.localhost:8443` by default; set `GITONE_E2E_URL` to
override it. The test browser accepts the generated local certificate, without
changing application TLS verification or the host trust store. Test screenshots,
traces and Playwright MCP artifacts are excluded from Git; treat them as private
because authentication failures may capture temporary OIDC parameters.

The suite uses real Keycloak authorization-code redirects and two separate
browser contexts. It covers username registration and conflicts, login/logout,
group creation, invitation discovery and acceptance, role changes, immediate
revocation, invitation cancellation, the last-owner safeguard, owner departure,
and mobile navigation. Repository tests cover creation in personal and shared
spaces, duplicate names, empty repositories, README/file/branch/history views,
reader write denial, private access, and login deep links. No authentication,
group, or repository APIs are mocked.

See [the Compose guide](../deploy/compose/README.md) for demo accounts, local CA
trust, resource requirements, and persistence. Test-created namespaces and
repositories remain in the local MinIO data, just like normal user data.
