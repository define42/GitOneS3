# Shared-space discovery and migration

`GITONE_SPACE_DISCOVERY_MODE` controls reads: `indexed` (the default) or `scan`
(the upgrade compatibility mode). Both modes write discovery candidates before
committing new group membership or invitations. The mode is not part of the
immutable routing identity; changing it does not change shard assignments.

New installations initialize an index automatically only when the shard has no
namespace records. An existing shard without a valid readiness marker refuses
to start in indexed mode. Startup does not silently backfill existing records:
older servers could still be writing groups without index entries.

## Existing installations: two-phase rollout

1. Deploy this release with `GITONE_SPACE_DISCOVERY_MODE=scan` on **every shard**.
   Finish replacing all old writer processes, including draining their in-flight
   requests. Verify each running image/version and mode before continuing. New
   writes now maintain the index while discovery still reads namespace records.
2. Run `gitone backfill-space-index` once for **each configured shard**, using
   that shard's normal environment, credentials, and mounted cluster identity.
   The command requires scan mode, opens only that shard's bucket, and does not
   start listeners or perform OIDC discovery. It backfills current memberships
   and pending invitations, then records completion. Run only one backfill per
   shard at a time. If interrupted or unsuccessful, leave the shard in scan mode
   and rerun it; backfill is idempotent and a failed run does not mark it ready.
3. After every shard succeeds, switch all shards to `indexed` and replace/restart
   the serving processes. Verify discovery and invitations after the rollout.

The command cannot verify that all other processes were upgraded. The operator
must enforce that barrier. Do not run backfill alongside an older writer, and
do not use an old ready marker to skip a backfill after old code has written data.
Backfill invalidates the previous readiness marker before rebuilding; it is not
an online repair command for a cluster currently serving indexed reads.

Current index-writing servers can continue serving in scan mode during backfill.
The backfill reads records in bounded storage pages. Concurrent additions remain
discoverable because their writers prewrite their own candidates. Concurrent
removal can leave a stale candidate, but authoritative membership checks exclude
it from results.

### Helm

Set `spaceDiscoveryMode: scan` together with the new image in your existing Helm
values, then upgrade the release. The chart uses `OnDelete`: changing the
StatefulSet template does **not** replace old pods automatically. Replace pods in
controlled batches and confirm every ordinal is running the new image and scan
mode before starting any backfill.

Run this for each ordinal, with the namespace and ordinal adjusted to your
installation:

```sh
kubectl -n gitone-system exec gitone-0 -- /usr/local/bin/gitone backfill-space-index
```

After all shards report success, set `spaceDiscoveryMode: indexed`, upgrade the
release, and replace pods again. Preserve all unrelated Helm values, secrets,
the shard count, and immutable cluster identity throughout both phases.

### Docker Compose

Fresh stores still start with `make run`. For existing persistent volumes, run
from the repository root:

```sh
GITONE_SPACE_DISCOVERY_MODE=scan make run
docker compose ps
# Confirm all four GitOne containers are the newly built release and healthy.
for shard in 0 1 2 3; do
  docker compose exec -T "gitone-$shard" /usr/local/bin/gitone backfill-space-index || exit 1
done
GITONE_SPACE_DISCOVERY_MODE=indexed make run
```

`make run` never performs a backfill automatically and never deletes persistent
data. If a backfill fails, keep scan mode until the cause is fixed and that
shard's command succeeds.

### Rollback

For a read-path rollback, keep this index-writing release and set scan mode.
Rolling back to an older binary that does not write candidates invalidates the
index's completeness even if its readiness marker still exists. Before allowing
such an old writer to run, stop indexed serving across the cluster. To return to
indexed mode later, repeat the full writer rollout and backfill on every shard.

## Storage and query behavior

Candidates live in the **group-owning shard's bucket** at:

```text
auth/space-index/v1/users/<hex-sha256-user-id>/<group>.json
auth/space-index/v1/ready.json
```

The user ID is the stable provider identity, not a caller-selected username.
Candidate entries identify groups only; membership, invitations, roles, and
authorization remain authoritative in `auth/users/<group>.json`. Listing derives
the user's index prefix from the verified session and filters every candidate
against the current group record. A failed group update may leave an extra
candidate, but cannot grant access through that candidate.

Removal and invitation cancellation do not delete candidates. This avoids a
concurrent removal/reinvite race that could otherwise hide valid membership.
Do not add lifecycle expiration to this prefix or manually prune markers based
only on a membership snapshot. Cleanup requires a separate coordinated design.
Discovery work scales with the user's current **and historical** candidate groups
on each shard, rather than all namespace records in the shard.

The spaces API accepts `limit` (1–100, default 100) and an opaque `cursor` and
returns `nextCursor` when more candidates remain. Continue until `nextCursor`
is absent, even if an intermediate `spaces` array is empty after filtering stale
candidates. Cursors are scoped to the authenticated user, shard, and discovery
mode; restart pagination after switching modes. Pagination is not a point-in-time
membership snapshot: fresh authorization checks still govern every operation.
