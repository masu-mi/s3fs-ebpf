#!/usr/bin/env bash
# direct PUT 100 MiB x N to MinIO, log ms + /proc/meminfo Dirty/Writeback + disk util
C=s3fs-ebpf-minio
docker exec $C mc mb --ignore-existing local/burst >/dev/null
head -c $((100*1024*1024)) /dev/urandom > /tmp/scratch/p100
for i in $(seq 1 40); do
  t=$EPOCHREALTIME
  docker exec -i $C mc pipe local/burst/o-$((i%4)) < /tmp/scratch/p100 >/dev/null
  ms=$(awk -v a=$t -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}')
  d=$(awk '/^Dirty:/{d=$2}/^Writeback:/{w=$2}END{printf "dirty=%dMB wb=%dMB",d/1024,w/1024}' /proc/meminfo)
  echo "$i ${ms}ms $d"
done
docker exec $C mc rb --force local/burst >/dev/null
