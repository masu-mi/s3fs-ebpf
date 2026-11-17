#!/usr/bin/env bash
C=s3fs-ebpf-minio
docker exec $C mc mb --ignore-existing local/burst >/dev/null
head -c $((100*1024*1024)) /dev/urandom > /tmp/scratch/p100
for i in $(seq 1 24); do docker exec -i $C mc pipe local/burst/seed-$i < /tmp/scratch/p100 >/dev/null; done
sync; sleep 3
t0=$EPOCHREALTIME
docker exec $C mc rm --recursive --force local/burst >/dev/null
echo "rm took $(awk -v a=$t0 -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}')ms"
for i in $(seq 1 30); do
  t=$EPOCHREALTIME
  docker exec -i $C mc pipe local/burst/o-$((i%4)) < /tmp/scratch/p100 >/dev/null
  echo -n "$(awk -v a=$t -v b=$EPOCHREALTIME 'BEGIN{printf "%.0f",(b-a)*1000}') "
done; echo
docker exec $C mc rb --force local/burst >/dev/null; rm -f /tmp/scratch/p100
