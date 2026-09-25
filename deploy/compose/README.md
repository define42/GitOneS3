# Local four-shard stack

Run from the repository root:

```sh
make run
```

Requires Docker Engine/Desktop with Compose **2.24+**, Make, and free local ports
8443, 9000 and 9001. No host Go, Python, Keycloak, MinIO, cloud account, or manual
bucket/realm setup is needed. The first build needs internet access and can take
several minutes (allow about 6 GB RAM for building MinIO and running the stack).
Later starts reuse the build cache and persistent data.

The command generates local secrets and TLS material, builds the images, creates
four S3 buckets, imports the Keycloak realm, and waits for all services to become
healthy. These are four Compose containers representing the Kubernetes shard
pods; each still listens on **one HTTP port, 8080** inside the network.

## Open and log in

| Service | Address |
| --- | --- |
| GitOne | <https://gitone.localhost:8443> |
| Register a GitOne username | <https://gitone.localhost:8443/auth/register> |
| Sign in | <https://gitone.localhost:8443/auth/login> |
| Git access tokens | <https://gitone.localhost:8443/auth/tokens> |
| API documentation | <https://gitone.localhost:8443/api/docs> |
| Keycloak admin | <https://keycloak.gitone.localhost:8443/admin/> |
| MinIO S3 | <http://localhost:9000> |
| MinIO console | <http://localhost:9001> |

Demo realm accounts are `alice` / `alice-dev-password` and
`bob` / `bob-dev-password`. They have verified example email addresses, require
no setup steps, and use authorization code flow with PKCE (password/direct grant
is disabled). In the GitOne UI, register an available username and sign in to
Keycloak with either demo account. The GitOne username is your choice; it does
not have to match the Keycloak login. Registration binds that username to the
Keycloak subject permanently. Later choose **Sign in** with the registered
GitOne username and the same Keycloak account.

From your personal space, create a shared group using an available name.
The creator becomes its owner. Invite another registered GitOne username as
reader, developer, or owner; the invitee accepts from their invitations before
gaining access. Owners can change roles and remove members, but cannot remove
or demote the last owner. **Sign out** clears the GitOne browser session; it
does not end Keycloak's separate SSO session.

Use **New repository** to create a private repository in your personal namespace
or a group where you are a developer or owner. Optional README initialization
writes real Git objects to MinIO, and the UI can browse branches, files, and
commit history. Group readers can browse but cannot create repositories. Create
a repository-scoped token under **Access tokens** to clone, fetch, pull, or push
using native Git; use your GitOne username and the token as the password.
Git LFS still returns `501 Not Implemented`.

Keycloak's admin username is `admin`; its generated password is in
`.local/keycloak.env`. MinIO's username is `gitone-local`; its generated password
is in `.local/minio.env`. These files contain secrets: do not paste or commit them.
The stack is **development-only**, with public demo passwords and Keycloak's
embedded development database. Published ports bind only to `127.0.0.1`.

### Local HTTPS

`make run` generates `.local/tls/ca.crt` and certificates for both local names.
Import **only `ca.crt`**, never its private key, into your browser's trusted
certificate authorities. This is an explicit local trust decision; `make run`
does not modify your OS/browser trust store. Until then, the browser will show a
certificate warning on both GitOne and Keycloak. All containers already trust
the generated CA; TLS verification is never disabled and cookies stay
Secure/HttpOnly.

Modern browsers resolve `*.localhost` to the local machine. If your browser or
system resolver does not, add this entry using your normal hosts-file editor:

```text
127.0.0.1 gitone.localhost keycloak.gitone.localhost
```

Host command-line probes can avoid any resolver changes:

```sh
curl --noproxy '*' --cacert .local/tls/ca.crt \
  --resolve gitone.localhost:8443:127.0.0.1 \
  https://gitone.localhost:8443/readyz
```

For Git, resolve `gitone.localhost` locally as described above and trust the
generated CA explicitly. Run from the GitOne source checkout, replacing
`alice/project` with your existing repository:

```sh
git -c http.sslCAInfo="$PWD/.local/tls/ca.crt" \
  clone https://gitone.localhost:8443/alice/project.git
git -C project config http.sslCAInfo "$PWD/.local/tls/ca.crt"
```

Git prompts for the registered GitOne username and the token. Never put the
token in the URL or disable certificate verification. The local repository
setting preserves CA verification for later fetches and pushes. See the
[Git authentication guide](../../docs/git-authentication.md) for permission,
revocation, credential storage, and repository-size limits.

## Commands and persistence

```sh
make run                 # initialize, build, start in background, wait for health
make smoke               # real OIDC login + group invitation/access test
make test-ui             # Playwright browser flows (install test tools below)
make smoke-git           # native HTTPS Git + OIDC + MinIO (host Git/Python)
make logs                # follow service logs
make stop                # stop/remove containers; preserve data and keys
docker compose ps        # inspect all four GitOne instances and dependencies
make run-local           # old bare Go process; requires your own environment
```

The smoke test uses real Keycloak login forms, the fixed callback, MinIO-backed
claims and transactions, and shared cookies through the round-robin proxy.
It checks all four shard readiness endpoints, invites/accepts a member, enforces
reader permissions, and verifies immediate revocation. It creates uniquely
named `smoke-*` user/group namespace records, which remain in local storage.

The native Git smoke test additionally verifies clone, push, pull, an empty
repository's first push, agreement with browser file/history APIs, read-only
and revoked PATs, and group membership revocation across different owner shards.
It needs host Python 3.10+ and Git, trusts `.local/tls/ca.crt`, and handles local
name resolution itself. It does not print credentials, puts no token in a URL,
and revokes generated PATs during cleanup. Its uniquely named `git-*` namespaces
and repository data remain in MinIO; temporary Git working trees are removed.

For the browser suite, install Node.js 22.12+ on the host, then run:

```sh
npm --prefix web ci
cd web
npx playwright install chromium
cd ..
make test-ui
```

The Playwright configuration targets the running Compose stack and allows its
generated local certificate only in the test browser. Application TLS
verification remains enabled. Browser traces, screenshots, and MCP artifacts
are gitignored because failed OIDC navigation may contain temporary login
parameters, and token-management screenshots or traces can contain a new token.
The browser tests also create permanent, uniquely named local
namespace records. See [the UI guide](../../docs/browser-ui.md).

MinIO data and Keycloak users/subjects live in named Docker volumes. Secrets and
the local CA live in the gitignored `.local/` directory. Keep these together:
deleting only Keycloak's database changes user identities; deleting only
`.local/` loses client secrets and cookie keys while the imported realm remains.
Keycloak skips importing an already-existing realm on subsequent starts.
There is deliberately no automatic destructive reset target.

Login transaction objects expire after one day; user/group namespace records
never expire. Do not change the shard count, cluster identity, or bucket prefix
against existing data.

## Routing and authentication

NGINX terminates local HTTPS and balances GitOne requests across all four
instances. Compose DNS aliases reproduce the StatefulSet pod DNS names, so the
existing owner-shard forwarding works unchanged. The same Keycloak issuer URL
resolves to NGINX inside Compose and to loopback in the browser; discovery,
authorization, callback, token exchange and JWKS verification use that one
issuer, without split-issuer overrides or insecure TLS exceptions.

The realm imports one confidential client (`gitone`), an exact callback URI
`https://gitone.localhost:8443/auth/oidc/callback`, and the two demo users.
Application roles and group memberships remain in GitOne, not Keycloak roles.
The Keycloak proxy reserves a 16 KiB response-header buffer for SSO redirects,
which carry both signed callback state and multiple identity-provider cookies.

The MinIO image is built from the pinned patched community source release
`RELEASE.2025-10-15T17-29-55Z`. Upstream distributed that release as source, not
an updated official binary container. See the
[MinIO release notes](https://github.com/minio/minio/releases/tag/RELEASE.2025-10-15T17-29-55Z).
The source is AGPLv3; its license is included in the image.

References: [Keycloak containers and realm import](https://www.keycloak.org/server/containers),
[Keycloak reverse proxy configuration](https://www.keycloak.org/server/reverseproxy),
[Compose readiness ordering](https://docs.docker.com/compose/how-tos/startup-order/).
