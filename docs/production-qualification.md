# Qualify a deployment

Repository limits and local tests do not establish deployment capacity. Run the
following checks on the intended CPU, memory, temporary disk, network and S3
provider before accepting traffic. Keep the reports with the deployed revision
and configuration.

## S3 load and recovery test

`make qualify-s3` uses the production Git HTTP handler and S3 adapter against a
new, randomly named bucket. It deletes that bucket afterward. Use a dedicated
test account/profile with bucket create/delete permissions; the harness cannot
select an existing bucket. Credentials use the normal AWS SDK credential chain.

```sh
AWS_PROFILE=gitone-qualification \
AWS_REGION=us-east-1 \
GITONE_TEST_S3_ENDPOINT=https://s3.example.com \
GITONE_QUALIFY_CLIENTS=4 \
GITONE_QUALIFY_ROUNDS=10 \
GITONE_QUALIFY_MAX_SECONDS=30 \
make qualify-s3
```

The endpoint must be an HTTP(S) origin without a path, query or credentials.
The adapter uses path-style bucket addressing. Defaults are one client, three
rounds, a 63 MiB binary repository per client, and a 90-second latency budget.
`GITONE_QUALIFY_MIB` accepts 1–63 MiB for a quicker smoke check. Results include
each request's latency, response size and error, plus storage metrics, under
`tmp/qualification/` (override `QUALIFICATION_OUTPUT` if needed).

The harness reads the normal `GITONE_GIT_MAX_CONCURRENT_OPERATIONS`,
`GITONE_GIT_MAX_QUEUED_OPERATIONS`, and `GITONE_GIT_QUEUE_TIMEOUT` settings.
Their defaults remain one active operation, four queued, and five seconds of
queue wait. Any failed request or latency above the configured budget fails the
test. Exercise the expected burst with those defaults before raising concurrency;
more active transfers require more memory and temporary disk.

The test also reopens the repository through a new Store, verifies durable state,
checks that a crash lock fences a new writer, recovers that lock while serving is
stopped, restores a retained generation and checks Git integrity. A small S3
version of this test runs in CI.

For native Git checks near both limits, add:

```sh
GITONE_QUALIFY_CAPACITY=both make qualify-s3
```

This adds serial push, clone, native `git fsck`, and integrity checks for 1023 MiB
of binary blobs and exactly 100,000 objects. Use `bytes` or `objects` for one case.
It retains the real 90-second transfer deadlines. See
[performance measurements and resource bounds](git-performance.md).

The load harness uses loopback HTTP and bypasses authentication and ingress. Run
native HTTP and SSH clone/push, Git LFS upload/download, login, permission
revocation and browser navigation through the actual public deployment too.
Measure container memory including page cache, disk high-water usage, CPU,
client tail latency, HTTP rejection rates and
[storage calls, latency and bytes](storage-metrics.md). Include cancellations,
slow clients, overlapping LFS traffic and scheduled maintenance. A passing
small fixture alone is insufficient evidence for maximum-size repositories.

## Upgrade and recovery

Drain older readers and writers before upgrading; mixed versions are unsupported.
The LFS quota ledger has a specific
[migration and rollback procedure](git-lfs.md#upgrade-and-migration).

Each shard has one serving owner. Replacement causes a period of shard
unavailability. A crashed writer can leave a durable lock requiring
[offline recovery](repository-maintenance.md#recover-a-lock-after-a-crash).
Never unlock based only on age: a paused writer can resume. Practice the
documented stop, inspect, conditional unlock and integrity-check sequence, and
measure the recovery time against the deployment's availability requirement.
Automatic uninterrupted failover needs a separate ownership/fencing design.

## Backup restoration

Retained-generation restore repairs Git history while its S3 artifacts remain
available. It does not replace a complete backup. Back up the immutable cluster
identity, authentication and namespace records, repository metadata/publication
records, Git artifacts, LFS payloads and verified records, and required secrets.
Do not configure age-based expiration for completed repository objects.

Practice restoration into an isolated environment using the provider's supported
backup/version-recovery mechanism. Preserve the cluster identity and the stored
version relationships: LFS verified records pin payload ETags, so a generic copy
that changes an ETag is not automatically a valid restore. Keep writers stopped
while restoring mutually dependent records. Recover abandoned locks only after
fencing the previous processes, following the offline procedure above.

Before reopening a restored deployment, verify namespace ownership and revoked
credentials, run `gitone repository check` for every restored repository, clone
with native Git and run `git fsck --full --strict`, and fetch and hash referenced
LFS content. Record the achieved recovery point and elapsed recovery time.
Provider backup restoration and ingress behavior require deployment evidence;
the local qualification harness does not simulate them.
