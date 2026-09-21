# GitOne Helm chart

This chart creates one `gitone-N` StatefulSet pod per permanent routing shard,
one public Service, and one headless Service for direct shard forwarding. The
StatefulSet replica count and the mounted cluster identity both come from
`shardCount`; there is no independent replica setting.

The chart does not create S3 credentials. The base StatefulSet shares one
ServiceAccount across every ordinal, so `serviceAccount.annotations` alone
cannot provide per-shard bucket IAM. For strict bucket-scoped credentials, use
an ordinal-aware admission/credential broker via `podAnnotations`, or deploy a
shard-specific workload topology outside this generic chart. Restrict
`networkPolicy.externalEgress` to each S3, metadata, identity, or broker
destination and port used by the installation.
Choose a globally unique `s3.bucketPrefix`; the default is suitable only as a
development/example value.

Internal forwarding uses application-layer `http` without a shared token or
caller authentication. A forwarding marker prevents repeated hops. Transparent
service-mesh mTLS can be configured independently if desired.

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
