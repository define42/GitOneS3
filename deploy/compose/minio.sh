#!/bin/sh
set -eu
export AWS_PAGER=""
for shard in 0 1 2 3; do
    bucket="gitone-local-shard-${shard}"
    if ! aws --endpoint-url http://minio:9000 s3api head-bucket --bucket "$bucket" 2>/dev/null; then
        aws --endpoint-url http://minio:9000 s3api create-bucket --bucket "$bucket"
    fi
    aws --endpoint-url http://minio:9000 s3api put-bucket-lifecycle-configuration \
        --bucket "$bucket" --lifecycle-configuration file:///init/lifecycle.json
done
