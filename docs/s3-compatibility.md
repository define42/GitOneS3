# S3 endpoint compatibility checks

`gitone-s3check` tests an existing S3 bucket against the operations GitOne needs.
It runs independently of GitOne: no cluster identity, shard configuration, OIDC,
or running GitOne pod is required.

The tool reports observed behavior. A passing run cannot establish a provider's
consistency guarantee or prove atomicity under every race. Confirm the provider's
read/list consistency contract and run workload tests before production use.

## Build and run

```sh
make build-s3check
bin/gitone-s3check --help
```

Use an existing **test bucket whose versioning has never been enabled**. The tool
checks bucket versioning before writing and refuses enabled or suspended
versioning, because ordinary deletion would leave versions or delete markers.
It does not create buckets or change bucket settings.

Credentials use the AWS SDK's normal provider chain: environment variables,
shared credentials profiles, or workload credentials. There are no credential
flags. Configure `AWS_PROFILE`, or supply `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and optionally `AWS_SESSION_TOKEN` through your normal
secret management.

For DigitalOcean Spaces, use the regional S3 origin and its region:

```sh
AWS_PROFILE=spaces-test bin/gitone-s3check \
  --endpoint https://nyc3.digitaloceanspaces.com \
  --region nyc3 \
  --bucket gitone-compatibility-test \
  --list-api both
```

For local MinIO, use its existing test bucket and a credentials profile:

```sh
AWS_PROFILE=minio-test bin/gitone-s3check \
  --endpoint http://localhost:9000 \
  --region us-east-1 \
  --bucket gitone-compatibility-test \
  --path-style
```

These commands require the named bucket and credentials profile to exist. Use
the same provider endpoint, addressing style, and permissions intended for
GitOne. The tool talks directly to storage as an operator diagnostic; Git and
LFS client traffic continues to go through GitOne pods.

## What it checks

- Conditional creation with `PUT If-None-Match: *`.
- Conditional replacement and deletion with `If-Match`, including stale ETags.
- Concurrent conditional operations, checking that conflicting contenders
  cannot both succeed.
- Immediate reads and listings after object changes.
- Complete ordered pagination over more than 1,000 objects: legacy
  `ListObjects` markers, `ListObjectsV2` continuation tokens, and V2 `StartAfter`.
- Byte ranges, multipart upload completion and abort, and upload checksum
  handling used by GitOne's streaming storage path.
- Cleanup of the exact keys and multipart uploads created by the run.

By default, all listing modes are checked. `--list-api v1` checks only legacy
listing; `--list-api v2` checks both V2 pagination approaches. All the other
storage checks still run in either mode.

**Passing the V1 checks does not enable V1 listing in GitOne.** The production
S3 adapter currently uses V2, so an endpoint that passes V1 and fails V2 still
requires a production adapter change. The report is a diagnostic result, not a
provider certification.

## Flags and reports

| Flag | Default | Purpose |
| --- | --- | --- |
| `--endpoint` | required | HTTP or HTTPS S3 origin without credentials, path, query, or fragment |
| `--bucket` | required | Existing bucket with versioning never enabled |
| `--region` | `us-east-1` | Request signing region; set explicitly for the provider |
| `--path-style` | `false` | Put the bucket in the request path |
| `--prefix` | `gitone-s3check/` | Parent prefix for a randomly named test run |
| `--list-api` | `both` | `both`, `v1`, or `v2` |
| `--objects` | `1005` | Number of tiny pagination objects, from 1,001 to 10,000 |
| `--timeout` | `10m` | Check deadline; cleanup has a separate two-minute deadline |
| `--json` | `false` | Emit a JSON report on stdout |

Each result contains a check name, `pass`/`fail`/`skip` status, detail, and
duration. The report identifies the bucket and isolated test prefix. Diagnostic
messages go to stderr, keeping JSON stdout suitable for automation:

```sh
AWS_PROFILE=spaces-test bin/gitone-s3check \
  --endpoint https://nyc3.digitaloceanspaces.com \
  --region nyc3 \
  --bucket gitone-compatibility-test \
  --objects 2505 \
  --json > spaces-report.json
```

| Exit code | Meaning |
| --- | --- |
| `0` | All selected checks and cleanup passed |
| `1` | Check, configuration, network, cancellation, or cleanup failure |
| `2` | Invalid command arguments |

SDK retries are disabled so the report does not hide an initial failed request.
A network interruption or throttling can therefore fail a run; inspect the
details and repeat after addressing the cause. `--help` requires no credentials
or network access.

## Permissions, resource use, and cleanup

Grant access to read bucket versioning and list the bucket, and to put, get,
delete, and abort multipart uploads under the selected test prefix. AWS policy
action names are `s3:GetBucketVersioning`, `s3:ListBucket`, `s3:PutObject`,
`s3:GetObject`, `s3:DeleteObject`, and `s3:AbortMultipartUpload`; providers may
express these permissions differently.

The default run creates 1,005 tiny pagination objects plus a small number of
other test objects. Multipart checks upload about 10 MiB and download about
5 MiB. Expect thousands of storage requests; provider request, storage, and
transfer charges apply.

Every run creates a random child below `--prefix`. The bucket and random run
prefix are printed to stderr before checks start, so they are available even
if the process is killed before it writes the final report. Cleanup tracks exact keys
and upload IDs, so a broken listing implementation cannot make it delete
unrelated objects or prevent it from identifying its own objects. Cancellation
by timeout, Ctrl-C, or SIGTERM still attempts cleanup using a separate deadline.
An abrupt process kill or unavailable endpoint can leave test artifacts; use
the reported run prefix to remove that run's remaining objects and incomplete
uploads. A cleanup failure makes the command fail.

The tool does not exercise cluster routing, migration, repository size limits,
provider quotas, long-running consistency behavior, or storage performance
under sustained load. Run it separately for each provider configuration that
will back a shard.
