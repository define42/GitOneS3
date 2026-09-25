# GitOne Helm chart

This chart creates one `gitone-N` StatefulSet pod per permanent routing shard,
one public Service, and one headless Service for direct shard forwarding. The
StatefulSet replica count and the mounted cluster identity both come from
`shardCount`; there is no independent replica setting.

## Shared-space discovery upgrades

`spaceDiscoveryMode` defaults to `indexed`. Existing stores without a completed
index refuse startup in this mode. Deploy the new image with
`spaceDiscoveryMode: scan`, replace **all** old writer pods (the StatefulSet uses
`OnDelete`), run `gitone backfill-space-index` on every shard, then switch to
`indexed` and replace pods again. Do not backfill while any older writer remains.
See the [migration and rollback procedure](../../../docs/space-discovery.md).

## Google OIDC

Enable authentication with:

```yaml
auth:
  enabled: true
  publicURL: https://git.example.com
  googleClientID: YOUR_CLIENT_ID.apps.googleusercontent.com
  existingSecret: gitone-google-auth
```

Provision that Secret independently with these keys:

- `google-client-secret`: the Google OAuth client secret.
- `cookie-hash-key`: base64 encoding of 64 random bytes.
- `cookie-block-key`: base64 encoding of 32 independently generated random bytes.

The chart injects the same Secret into every shard. It never generates keys at
render time. Register `https://git.example.com/auth/google/callback` with Google,
terminate HTTPS at ingress, and allow outbound HTTPS to Google's discovery,
token, and signing-key endpoints. Start login at
`https://git.example.com/<username>/auth/google/login`.
The first verified Google account to claim a username owns it permanently.

Set an S3 lifecycle rule for `auth/transactions/` to expire abandoned/consumed
login records after one day; retain `auth/users/` indefinitely, as it now holds
the common user/group namespace claims and group membership records. Existing
user records are read without migration. See the root README for session/CSRF,
group creation and sharing APIs, and current authorization limitations.

The chart does not create S3 credentials. The base StatefulSet shares one
ServiceAccount across every ordinal, so `serviceAccount.annotations` alone
cannot provide per-shard bucket IAM. For strict bucket-scoped credentials, use
an ordinal-aware admission/credential broker via `podAnnotations`, or deploy a
shard-specific workload topology outside this generic chart. Restrict
`networkPolicy.externalEgress` to each S3, metadata, identity, or broker
destination and port used by the installation.
Choose a globally unique `s3.bucketPrefix`; the default is suitable only as a
development/example value.

Client requests and internal forwarding use the same `service.publicPort`
(default `8080`). Forwarding uses application-layer `http` without a shared token or
caller authentication. A forwarding marker prevents repeated hops. Transparent
service-mesh mTLS can be configured independently if desired.

Remove the obsolete `service.internalPort` from custom values files when
upgrading from a two-port release.

When upgrading from a token-based release, remove `internalAuth` from custom
values files. Coordinate the shard update: older pods still require a token
and will reject forwards from updated pods.

`gitone-cluster-identity` is an immutable, Helm-retained ConfigMap. It pins the
shard/hash/canonicalization tuple, path limits, and S3 bucket mapping. Back it
up before first use. An uninstall leaves it behind; reinstall with the same
release name, namespace, and identity values so Helm can retain ownership.
After the first namespace is created, changing or deleting this identity
requires an explicit data migration. If it is accidentally lost, restore the
exact backup rather than rendering a new identity from guessed values.

The PodDisruptionBudget is off by default: with one owner pod per shard, any
permitted disruption makes that shard unavailable. StatefulSet updates use
`OnDelete` so operators can replace small batches and verify health between
batches.
## Optional SSH

Enable `ssh.enabled` with `auth.enabled`, set `ssh.publicURL` to your external
SSH origin, and provide `ssh.existingSecret` containing two distinct Ed25519
private keys named `host-key` and `forward-key`, shared by every shard. Keys
must persist across pod restarts. `ssh.port` defaults to 2222 inside pods and
`ssh.servicePort` defaults to 22 on the public Service. Use a TCP load balancer;
the HTTP ingress cannot proxy SSH. The chart opens the SSH listener in its
NetworkPolicy and mounts the secret read-only. See
[SSH configuration and trust model](../../../docs/git-ssh.md).
