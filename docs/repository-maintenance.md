# Repository maintenance

The `gitone repository` commands operate directly on one repository in the
configured shard bucket. They use the normal environment, AWS credentials, and
mounted immutable cluster identity. They do not start HTTP/SSH listeners or
contact an OIDC provider.

Use canonical `namespace/repository` names without a `.git` suffix. A command
checks ownership before opening storage and tells you which `gitone-N` owns a
target on another shard. Storage access then uses the same identity validation
and conditional-operation checks as the server.

## Prepare the deployment

Before allowing any push or repack with this release, drain **all older readers
and writers**, then replace them across the cluster. Include auxiliary processes
that serve or modify repositories. A new push can publish a schema 2 manifest;
older binaries only understand schema 1 and cannot read the updated repository.
Serving a mixed fleet is unsupported. Resume traffic after every process uses
this release. Rolling back to an older binary after a packed publication is also
unsupported.

Garbage collection has a separate concurrency requirement: every writer must
honor the new durable repository lock. Older writers do not acquire it, so they
must remain stopped during GC, restore, and repack. This also applies to a GC dry
run, whose candidate report assumes exclusive access to writers. The CLI cannot
verify that either rollout requirement has been met.

Run commands with the owning shard's `POD_NAME`, `POD_NAMESPACE`,
`GITONE_SHARD_COUNT`, cluster identity file, and bucket credentials. Credentials
need the same read, write, list, and conditional-delete capabilities as serving.
Even checks run the startup storage capability probes.

The object store must provide strongly consistent reads and prefix listings,
in addition to atomic conditional writes and deletes. GC discovers retained
state snapshots through listings; an incomplete listing can omit a historical
root and make its artifacts appear unreferenced. The startup probe checks
conditional operations and immediate listing visibility after creating,
updating, and deleting one object. Passing that probe does not establish a
provider's consistency guarantees. Verify those guarantees before using GC with
a compatible provider.

Temporary files go under `TMPDIR`; the Helm chart uses
`/var/lib/gitone/work` on its workspace volume. Allow about 4 GiB of temporary
space per active Git transfer at the configured maximum limits, plus headroom
and space for concurrent maintenance. An integrity check can cache up to 1 GiB
plus 8 MiB of packs on disk. Repack can hold that cache and an output pack of the
same maximum size. Filesystem metadata and temporary files left by interrupted
commands also consume space. The repository remains authoritative in S3; these
files can be regenerated.

Object bodies are read individually, but manifests, graph indexes, and artifact
listings still use RAM. A manifest supports at most 100,000 objects; maintenance
listings stop at 1,000,000 artifacts per repository. These bounds do not impose a
process memory limit. Maintenance commands run as separate processes and do not
share the server's transfer admission limit, so include them in the deployment's
memory and disk budget.

Normal completion removes temporary workspaces. An abrupt process termination
can leave files behind. Stop all processes using the workspace before removing
stale GitOne temporary files; deleting a live workspace can interrupt an active
transfer or maintenance operation.

## Check and inspect generations

```sh
gitone repository check alice/demo
gitone repository generations alice/demo
```

`check` verifies the current generation's immutable snapshot digests, object
digests, and Git graph connectivity. A successful report includes the generation,
object/reference/pack counts, and total decoded object bytes.

`generations` lists retained immutable state snapshots. Each entry includes an
exact `snapshot` key, generation number, timestamp, default branch, and whether
it matches the current published state. A listed snapshot can be an abandoned
publication proposal. Multiple proposals may have the same generation number;
only `current: true` proves which snapshot is currently published. Choose an
exact snapshot key for restoration.

## Restore a retained generation

After selecting a snapshot from `generations`, pass its full key:

```sh
gitone repository restore alice/demo --snapshot '<exact-snapshot-key>'
gitone repository check alice/demo
```

Restore verifies the selected generation's objects and graph before publishing
its references and manifest as a **new generation**. Existing generation history
is retained. This restores repository contents and Git references; it does not
restore deleted S3 objects, namespace ownership, or authentication records.
Recover externally deleted data from your S3 backup or object-versioning process
before using the command.

The repository metadata and current state publication object must remain valid.
Restore can recover a good retained generation when the current refs or manifest
are damaged, but it cannot reconstruct a missing or damaged publication object.

## Collect orphan artifacts

Start with a dry run:

```sh
gitone repository gc alice/demo
gitone repository gc alice/demo --grace-period 48h
```

The report gives candidate counts/bytes, retained generations, and artifacts
younger than the grace period. The default grace period is 24 hours; zero selects
the default. A positive Go duration such as `48h` overrides it. Deletion requires
an explicit flag:

```sh
gitone repository gc alice/demo --grace-period 48h --apply
```

Collection holds the same durable lock as writers, validates every retained
generation before deleting anything, and deletes candidates with their observed
object version as a precondition. Unknown object layouts, recently written
artifacts, and artifacts with no trustworthy modification time are retained.

Both dry runs and deletion hold the writer lock while scanning every retained
generation. Large histories can require substantial S3 reads and keep pushes
waiting or returning a retry response for the duration. Readers can continue
using their pinned generations. A corrupt retained generation prevents all
deletion, even if the current generation is healthy.

**All generation snapshots and everything they reference are retained.** This
keeps already-open readers valid and supports restoration of deleted branches
and earlier contents. It also retains objects referenced by abandoned publication
proposals. Collection currently reclaims orphans such as uploads that failed
before a retained state snapshot referenced them. It does not expire repository
history or scan unknown repository IDs left by failed repository creation.

A failure during deletion returns a nonzero exit status and still prints the
partial report, including completed deletions. Review the error and rerun the
dry run after resolving it. The command does not claim to roll back successful
deletions.

## Repack existing objects

```sh
gitone repository repack alice/demo
gitone repository check alice/demo
```

Repack writes the current reachable objects into one immutable canonical pack
and publishes a new generation. It supports migration from the original loose
object layout and consolidation of current packs. Old generation snapshots
retain their original artifacts, so repacking alone does not reduce retained
S3 history size.

## Recover a lock after a crash

Locks never expire automatically. This prevents a paused writer from resuming
after garbage collection has started. A process crash or failed lock release can
therefore leave a repository unable to accept writes or maintenance mutations.

1. Stop **every process using the repository**, including the owning shard,
   old/replacement pods, and other maintenance commands. Confirm that a paused
   or partitioned former writer cannot resume. Keep those processes stopped.
2. Inspect the lock using the same shard environment:

   ```sh
   gitone repository lock alice/demo
   ```

3. Copy the exact token from the report and explicitly acknowledge the offline
   condition:

   ```sh
   gitone repository unlock alice/demo --token '<exact-lock-token>' --offline
   gitone repository check alice/demo
   ```

4. Resume serving after the check succeeds.

The `lock` command only inspects a lock; it does not acquire one. `unlock`
compares the token and uses a conditional delete of the observed lock version.
The token is a recovery identifier, not a substitute for bucket authorization.
If the lock changes, investigate the process still using the repository.

## Output and exit status

Every executed maintenance operation prints one JSON object to stdout:

```json
{"operation":"check","namespace":"alice","repository":"demo","report":{"repositoryId":"...","generation":1,"references":1,"objects":3,"bytes":256,"packs":0}}
```

Failures can include an `error` field and a partial `report`; scripts must also
check the exit status. Logs and diagnostics go to stderr. Exit status `0` means
success, `1` means configuration/storage/operation/output failure, and `2` means
invalid command arguments. Argument and configuration failures can happen before
a report exists. JSON `gracePeriod` values use Go duration nanoseconds.

Flags can appear before or after the target. Unknown, duplicate, or inapplicable
flags and malformed targets are rejected before loading environment configuration.
Use `gitone repository --help` for a concise command reference.
