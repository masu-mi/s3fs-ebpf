# bpf_arena: Appropriate Use Cases & Design Guidelines

## Overview

`bpf_arena` (kernel 6.8+) provides zero-copy kernel↔userspace shared memory for eBPF programs, with a practical limit of **128MB–256MB** per arena due to kernel virtual memory allocation constraints. This document codifies appropriate and inappropriate use cases based on memory scaling and access patterns.

## Appropriate Use Cases

### 1. Metadata Caches (100KB–10MB)

**Profile:** Small, hot, frequently-accessed reference data.

- **Flow/connection state tables**: TCP tuples, connection aging info, per-flow counters
- **Policy decision tables**: Classifier rules, ACL lookups, rate-limit buckets
- **Routing/ARP caches**: Route table snapshots, neighbor table mirrors

**Memory estimate:** 10,000 entries × 100 bytes = 1MB  
**Access pattern:** lookup-heavy, infrequent mutations  
**Benefit:** Zero-copy lookup eliminates syscall/IPC latency; fits entirely in arena  
**Example:** `s3fs-ebpf` admit-metadata table (inode→location) without page data — proposed only. The implemented pcache mode keeps that table in the kmod admit hash and does not use the arena (2026-10-21)

### 2. Statistics & Histogram Aggregation (10MB–50MB)

**Profile:** Write-intensive counters/histograms that need userspace flush without syscall.

- **Request histograms**: latency percentiles, size buckets, error classification
- **Anomaly detectors**: CMS sketches, HyperLogLog, frequency ceilings
- **Per-core stats**: lock-free updates via `__sync_fetch_and_add`, userspace reads

**Memory estimate:** 100 histogram buckets × 1000 cores × 50 bytes = 5MB  
**Access pattern:** high-frequency writes in kernel, batched reads in userspace  
**Benefit:** BPF stats programs avoid `perf_output` buffering; userspace mmap-reads stats directly  
**Example:** Kernel-side request metrics aggregator feeding daemon metrics endpoint

### 3. Ring Buffers for High-Frequency Events (10MB–100MB)

**Profile:** Event logs where throughput overhead dominates, not total capacity.

- **Tracer rings**: syscall events, page fault logs, I/O completion events
- **Packet mirror rings**: sampled network packets for flow analysis
- **Kernel anomaly logs**: OOM events, throttle points, latency spikes

**Memory estimate:** 16 pages × 256KB ring per CPU × 64 CPUs = 256MB (exceeds practical arena; use multiple arenas)  
**Access pattern:** append-only writes in kernel, sequential drain in userspace  
**Benefit:** Ring buffer zero-copy avoids `perf_output` copying; no data loss on high frequency  
**Example:** S3 read pattern sampler (if it must log every read)

---

## Inappropriate Use Cases

### ❌ Bulk Data Caching (>100MB working set)

**Why:** Arena size fixed at creation time; scaling requires multiple arenas and userspace coordination complexity.

- Large file page cache (~GB range for streaming workloads)
- Disk cache (should use page cache directly)
- Database result buffers (use mmap or page cache)

**Lesson from s3fs-ebpf:** 128MB arena forced 4MB chunks with per-chunk lookups; large file (100MB) throughput regressed vs. page cache.

### ❌ Transient Buffers in Hot Paths

**Why:** Single shared arena across all CPU cores serializes on mutation; no per-core isolation.

- Socket buffers (use sk_buff ring, not arena)
- TX/RX ring buffers for NIC drivers (use packet memory pools)
- Scratch space for BPF programs (use stack/local storage)

### ❌ Unbounded or Variable-Size Collections

**Why:** Memory layout must be known at arena creation; resizing requires userspace coordination and program reload.

- Dynamic queues that grow on demand
- Collections where max size is unknown
- Sparse data structures (e.g., hash tables with unknown load factor)

---

## Design Principles

| Dimension | ✅ Good Fit | ❌ Poor Fit |
|-----------|-----------|-----------|
| **Total size** | ≤ 50MB | > 100MB |
| **Entry size** | 10–1000 bytes | > 10KB per entry |
| **Access frequency** | > 1M/sec | < 100/sec (syscall overhead not critical) |
| **Mutation rate** | Write/sec < 1% of reads | High write, low read |
| **Lifetime** | Fixed layout at creation | Dynamic resizing needed |
| **CPU cores** | Lock-free MPSC (e.g., histogram) | Global locks per arena |

---

## Memory Scaling Limits

| Scenario | Max Arena Size | Constraint | Notes |
|----------|----------------|-----------|-------|
| Single arena, small entries | 256MB | Kernel VM allocation, hugepage availability | Practical ceiling on most systems |
| Multiple arenas (per-function) | 1GB+ total | Userspace coordination, program complexity | Each arena is independent; no auto-rebalancing |
| With hugepage backing | 1GB per arena | NUMA topology, memory fragmentation | Requires kernel 6.10+, CONFIG_HUGETLB |

**Recommended ceiling for production:** 128MB per arena. Larger deployments should split by subsystem (stats arena, metadata arena, trace arena).

---

## s3fs-ebpf: Why Arena Failed Here

The project used arena to cache **page data** (128MB capacity holding ≤30 files). Problems:

1. **Size mismatch:** Working sets scale with S3 object count, not fixed at deployment
2. **GUP per chunk:** Access pattern required kernel-userspace round-trips per 4MB chunk
3. **No metadata-only path:** Committed to arena storage instead of delegating data to page cache

**Lesson:** If arena is 100MB and working set is 1GB, the overhead of multi-chunk access negates zero-copy benefit. Page cache was the right choice for bulk data; arena should have held only the admission table (metadata).

---

## Recommended Next Steps

1. **If using arena for metadata:** Benchmark lock-free histogram aggregation in arena vs. perf_output; quantify syscall elimination benefit
2. **If using arena for bulk data:** Measure GUP latency per page; if > 1µs, switching to page cache likely faster
3. **If scaling beyond 128MB:** Prototype per-function arena separation (stats, trace, metadata independent); measure coordination overhead

