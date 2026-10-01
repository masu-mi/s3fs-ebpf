#!/bin/sh
set -eu

ALIAS=local
BUCKET=${BUCKET:-s3fs-test}

mc alias set ${ALIAS} http://minio:9000 minioadmin minioadmin
mc mb --ignore-existing ${ALIAS}/${BUCKET}
mc anonymous set download ${ALIAS}/${BUCKET} || true

# Seed test objects used by scripts/bench-*.sh.
# Idempotent: skip upload if the object already exists.
# Use /dev/zero (not urandom) to keep init fast and avoid entropy exhaustion.
seed() {
	local obj="$1" size_mb="$2"
	mc stat ${ALIAS}/${BUCKET}/${obj} >/dev/null 2>&1 && return
	if [ "${size_mb}" = "0" ]; then
		printf 'Hello, s3fs-ebpf!\n' | mc pipe ${ALIAS}/${BUCKET}/${obj}
	else
		dd if=/dev/zero bs=1M count="${size_mb}" 2>/dev/null \
			| mc pipe ${ALIAS}/${BUCKET}/${obj}
	fi
	echo "  seeded ${obj}"
}

seed hello.txt 0
seed big.bin   1
seed 10m.bin   10
seed 100m.bin  100

echo "MinIO bucket ${BUCKET} ready (hello.txt, big.bin, 10m.bin, 100m.bin seeded)."
