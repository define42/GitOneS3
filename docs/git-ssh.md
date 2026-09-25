# Git over SSH

SSH is optional and complements HTTPS/PAT and browser OIDC authentication.
It supports clone, fetch, pull, and push using the same bounded S3 Git engine.
Create a repository in the UI first: pushing does not create repositories.

## User setup

Generate a key locally (prefer a passphrase and your local SSH agent):

```sh
ssh-keygen -t ed25519 -C "GitOne laptop"
```

In **SSH keys** (`/auth/ssh-keys`), name the key and paste the `.pub` file.
Never upload your private key. Ed25519, ECDSA, and RSA keys of 3072–8192 bits
are accepted. Certificates, DSA, authorized_keys options, and multiple keys
in one submission are rejected; SHA-1 authentication signatures are disabled.
The UI displays the SHA-256 fingerprint and supports revocation.

Select **SSH** on the repository page and copy its clone URL:

```sh
git clone ssh://alice@git.example.com:2222/team/project.git
```

Always use your own GitOne username (`alice`), including for group repositories.
Do not use `git`, the group name, your email, or an OIDC password as the username.
Verify the server's host-key fingerprint with the operator before trusting it.
For a non-default private key, use a local SSH host configuration or:

```sh
GIT_SSH_COMMAND='ssh -i /path/to/private-key -o IdentitiesOnly=yes' git clone ssh://alice@git.example.com:2222/team/project.git
```

Keys identify users, not repository scopes: each key can access all repositories
the user currently has permission to access. Use fine-grained PATs if narrower
credential scopes are needed. Group readers cannot push; current group roles
and personal ownership apply identically to HTTPS.

## Shards and revocation

Any pod accepts public SSH. The username's shard verifies the registered key;
successful SSH authentication then proves possession of the private key.
The repository namespace selects the data shard, which verifies the key again
and checks current permissions. It repeats those checks immediately before
publishing a push. Positive key checks are not cached.

Cross-shard key checks and Git streams use SSH peer sessions, with a **separate
cluster forwarding key** and pinned server host key. A public client cannot
submit an unsigned username assertion. Forwarded Git commands terminate on the
repository owner and cannot be forwarded again. Peer destinations come only
from the deployment's shard resolver. All shards are in one trust boundary:
protect these private keys as cluster credentials.

Only public key records are stored in the user's S3 bucket, in one conditional
keyring object: `auth/ssh-keys/<username>.json`. Records bind to the registered
OIDC issuer and subject. Creation and revocation use compare-and-swap. There
are at most 100 lifetime key records per user, including revoked tombstones;
a revoked key cannot be re-added. Names are limited to 100 bytes, submitted
public keys to 4096 bytes, and the serialized keyring to 512 KiB.

Revocation prevents subsequent authorization checks, including the final push
check. It cannot recall downloaded data or make the permission check atomic
with a concurrent membership change. Authority failures fail closed.

## Operator configuration

| Variable | Value |
| --- | --- |
| `GITONE_AUTH_ENABLED` | `true`; browser authentication must be configured |
| `GITONE_SSH_ENABLED` | `true` (default `false`) |
| `GITONE_SSH_PORT` | Pod listener, default `2222`, distinct from HTTP |
| `GITONE_SSH_PUBLIC_URL` | External SSH origin, e.g. `ssh://git.example.com:2222` |
| `GITONE_SSH_HOST_KEY_FILE` | Absolute path to persistent Ed25519 host private key |
| `GITONE_SSH_FORWARD_KEY_FILE` | Absolute path to a distinct Ed25519 peer private key |

Both unencrypted private keys must be identical across shards and supplied via
read-only secret mounts. Generate them once and preserve them across restarts;
never derive them from cookie keys or bake them into an image. Rotate host keys
with an announced client known_hosts update; rotating peer keys requires a
coordinated rollout. Mixed key generations fail closed. Public fingerprints can
be derived from an operator-owned mode-0600 copy with
`ssh-keygen -y -f <key-file> | ssh-keygen -lf -`; do not change live secret-mount
permissions just to run this command.

This adds an SSH port, not a second HTTP listener. Expose it through a TCP
load balancer/Service; an ordinary HTTP ingress does not route SSH. SSH only
accepts exact `git-upload-pack 'namespace/repository.git'` and
`git-receive-pack 'namespace/repository.git'` commands (optional leading slash).
No shell, passwords, PTY, SFTP, arbitrary exec, TCP tunnels, or agent forwarding.

Connections have a 10-second handshake deadline and 90-second total deadline,
at most three authentication attempts, and one command per connection. Each pod
bounds accepted concurrent connections to 128. HTTP and SSH share the existing
one-operation Git admission slot and object/pack/ref limits. These are small
repository limits, not an unrestricted large-repository hosting engine.
Public and peer connections share the connection bound; saturation can reject
authority checks and Git operations until connections close. Deploy TCP-level
connection/rate controls where needed; there is no per-user rate limiter yet.

## Local development and tests

`make run` starts the four shards with SSH enabled at `localhost:2222`, routed
through pod 0 to the correct owner. It generates persistent host and forwarding
keys under `.local/ssh/`. The host-side `.local` directory is mode 0700; its
read-only SSH mount is readable by the container's non-root UID. Keep this
directory private. Production deployments should use managed secrets instead.

```sh
git clone ssh://alice@localhost:2222/alice/project.git
go test -race -tags=integration ./internal/gittransport ./internal/sshserver ./internal/auth
npm --prefix web run test:e2e
```

The native-client suite exercises real SSH routing across separate shard stores,
clone/push/fetch and denied operations. Browser tests exercise key management,
SSH clone links, revocation, validation, and responsive settings layouts.
