# P-04 Trace harness

Offline policy simulator that reads a workload trace and reports
per-policy hit rate — no kmod, no mount, no S3. Complements
`scripts/bench-hitrate.sh` for fast policy-algorithm iteration.

Runs via:

```
s3fs-ebpf dev trace-replay [flags]
```

## Why

`scripts/bench-hitrate.sh` runs the actual kmod + mount + daemon
cycle for each policy — **~30 s per policy** — because that's the
only way to exercise BPF struct_ops end to end. Great for real
numbers, painful for algorithm iteration.

`trace-replay` runs a Go-side simulator over the same access
sequence in **milliseconds** (200 events: ~1 ms; 10 000 events:
~9 ms). Trade-off: simulator ≠ live BPF, so numbers diverge from
bench. Use trace-replay for shape intuition, bench for real hit
rates before Meetup.

## Trace format

JSON Lines, one event per line. Blank lines and `# comments`
allowed. See `daemon/internal/trace/format.go`.

```
# workload trace — 30-file Zipf(α=0.7)
{"ts":0,"ino":100,"op":"read"}
{"ts":1000,"ino":101,"op":"read"}
{"ts":2000,"ino":100,"op":"read"}
```

Fields:

| field | type | note |
|---|---|---|
| `ts`  | uint64 | monotonic ns from trace start (reserved for future time-decay sims; ignored today) |
| `ino` | uint64 | cache key |
| `op`  | string | `"read"` (miss on empty cache) or `"write"` (invalidate) — writes are parsed but not yet simulated |

The recorder side (kmod ring buffer → daemon drain → JSON Lines
writer) is P-04 followup. For now the harness generates synthetic
Zipf traces matching `scripts/bench-hitrate.sh`'s Python generator
(same α, N, seed=42).

## Usage

### synthetic Zipf (default)

```
$ s3fs-ebpf dev trace-replay --policies=lru,sieve
# trace-replay: 200 events, softcap=8
# policy       accesses       hits     misses     rate
  lru               200         81        119   40.50%
  sieve             200         91        109   45.50%
```

Matches `bench-hitrate.sh` workload params (α=0.7, files=30,
accesses=200, softcap=8). Change with `--zipf=alpha,files,accesses`:

```
$ s3fs-ebpf dev trace-replay --zipf=0.9,30,10000
# trace-replay: 10000 events, softcap=8
  lru             10000       5967       4033   59.67%
  sieve           10000       6570       3430   65.70%
```

### recorded trace

```
$ s3fs-ebpf dev trace-replay --trace=/tmp/prod.jsonl --policies=lru,sieve
```

## Simulator policies vs BPF policies

The simulator has 2 policies today: `lru` (strict LRU) and `sieve`
(paper-precise second-chance clock). They diverge from the BPF
counterparts:

| policy | BPF impl | Simulator impl | Why the gap |
|---|---|---|---|
| `lru` | `policy_lru_ops.bpf.c` — approximate age-based FIFO with epoch bumps per admit_replace | strict doubly-linked-list LRU | BPF has no linked-list primitive; age proxy loses ordering info on multi-round eviction cascades |
| `sieve` | `policy_sieve_ops.bpf.c` — per-ino visited bit, cleared during on_evict round | paper hand-pointer clock over linked list | BPF version can only clear per admit_replace round, not per operation |

Result: **on the same Zipf(0.7)/30/200 workload**:

```
                simulator (ms)     live bench (30 s / policy)
lru               40.5 %              7.5 %
sieve             45.5 %             66.0 %
```

- `lru` — sim uses strict LRU, bench uses coarse epoch-age proxy.
  Sim ≫ bench.
- `sieve` — sim's per-op hand pointer is stricter than the BPF's
  per-round bit; bench's per-folio `on_hit` also fires per read
  which gives hot items more life. Bench ≫ sim.

**Use trace-replay to explore algorithm shape** (e.g., "is SIEVE
consistently better than LRU across α values?"). **Use
bench-hitrate.sh for the real numbers** (e.g., "how do these
policies actually perform in our stack?").

## Roadmap

- Add W-TinyLFU / S3-FIFO / freq2bit to the simulator so all 9
  built-ins line up with their BPF counterparts.
- Wire the kmod-side recorder: BPF_MAP_TYPE_RINGBUF for read/write
  events, daemon drains to `--trace-out=<path>`, `s3fs-ebpf
  trace-record` subcommand.
- Multi-seed averaging: `--seed=42,43,44` runs 3 traces and
  averages, drops policy variance noise.
- Cost model: assign per-miss latency (μs) from a distribution
  and report tail-latency by policy, not just hit rate.
