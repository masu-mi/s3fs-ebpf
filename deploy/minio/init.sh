#!/bin/sh
set -eu

ALIAS=local
BUCKET=${BUCKET:-s3fs-test}

mc alias set ${ALIAS} http://minio:9000 minioadmin minioadmin
mc mb --ignore-existing ${ALIAS}/${BUCKET}
mc anonymous set download ${ALIAS}/${BUCKET} || true

echo "MinIO bucket ${BUCKET} ready."
