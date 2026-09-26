# Storage metrics

Every shard records object-store operation counts, latency, bytes consumed, and
active operations. Metrics stay in memory with a fixed set of operation and
result labels; object keys, repository names, users, and credentials are never
labels. Counters reset when the shard process restarts.

## Enable scraping

Set `GITONE_METRICS_TOKEN` to a random bearer token containing 32–1024 ASCII
bytes. Hex or base64 encoding of 32 random bytes works. Empty or unset disables
the endpoint, which returns `404`. Invalid tokens fail startup without printing
the secret.

Send `Authorization: Bearer <token>` to `GET /system/metrics` on the shard's
normal HTTP port. Missing or incorrect authorization returns `401`. `HEAD` is
also supported. The reserved `system` namespace avoids collisions with user
repositories. Requests are served by the local shard and never forwarded.

For Helm, provision a Secret separately and reference it:

```yaml
metrics:
  existingSecret: gitone-metrics
  secretKey: metrics-token
```

Scrape **each pod directly**, since a scrape of the public load-balanced Service
can switch between shards and mix counter histories. Protect the bearer token in
transit with the cluster's trusted network or transparent TLS transport. For
example, this Prometheus job collects two shards:

```yaml
scrape_configs:
  - job_name: gitone-storage
    metrics_path: /system/metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/gitone-metrics-token
    static_configs:
      - targets:
          - gitone-0.gitone-headless.gitone.svc:8080
          - gitone-1.gitone-headless.gitone.svc:8080
```

Use the real namespace, headless Service, port, and complete shard list. Mount
the same token in Prometheus through your deployment's Secret integration.

## Exported series

| Metric | Meaning |
| --- | --- |
| `gitone_storage_operations_total{operation,result}` | Completed storage calls, separated by outcome |
| `gitone_storage_operation_duration_seconds{operation}` | Histogram of completed operation duration |
| `gitone_storage_transferred_bytes_total{operation}` | Bytes read from upload/download streams, including upload retries |
| `gitone_storage_active_operations{operation}` | Calls in progress, including open download bodies |

Operations are `put`, `get`, `get_range`, `head`, `delete`, `list`, and
`list_page`. Results are `success`, `not_found`, `already_exists`,
`precondition_failed`, `conflict`, `canceled`, `deadline_exceeded`,
`invalid_range`, and `error`. Expected conditional-write races and missing
objects can be distinguished from backend failures.

For downloads, bytes reflect actual reads rather than advertised object size.
The completion result and duration are recorded when the body is closed, so
streaming failures and close errors are included. An unclosed body remains
visible as an active operation. Upload bytes reflect reads performed by the
storage adapter, which may include retries or checksum reads, and are not a
provider billing measure.

## Example alerts and queries

Unexpected backend failures over five minutes:

```promql
sum by (instance) (rate(gitone_storage_operations_total{result="error"}[5m]))
```

P99 storage duration per operation and shard:

```promql
histogram_quantile(0.99,
  sum by (instance, operation, le) (
    rate(gitone_storage_operation_duration_seconds_bucket[5m])
  )
)
```

An example Prometheus alert for persistent backend failures:

```yaml
groups:
  - name: gitone-storage
    rules:
      - alert: GitOneStorageFailures
        expr: sum by (instance) (rate(gitone_storage_operations_total{result="error"}[5m])) > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: GitOne shard has persistent object-store failures
```

Add deployment-specific thresholds for `deadline_exceeded`, download latency,
and persistently high active operations after measuring normal traffic.
Offline maintenance commands report their own work in their JSON output;
their short-lived process counters are not exposed by the serving shard.
