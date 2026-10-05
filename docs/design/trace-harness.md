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
writer) is P-04 followup. The synthetic `--zipf` generator uses the
same α / N / seed=42 parameters as `scripts/bench-hitrate.sh`, but Go's
`math/rand` and Python's `random` produce **different sequences** for
the same seed. For a like-for-like reference, replay the trace the
bench actually used: `bench-hitrate.sh` writes it to
`$OUT/trace.jsonl` and replays it itself at the end of a run.

## Usage

### synthetic Zipf (default)

```
$ s3fs-ebpf dev trace-replay
# trace-replay: 200 events, softcap=8
# policy       accesses       hits     misses     rate
  lru               200         81        119   40.50%
  sieve             200         91        109   45.50%
  opt               200        121         79   60.50%
  noevict           200        171         29   85.50%
```

Uses `bench-hitrate.sh`'s workload params (α=0.7, files=30,
accesses=200, softcap=8) but not its sequence (see above). Change with
`--zipf=alpha,files,accesses`:

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

The simulator has 2 candidate policies today: `lru` (strict LRU) and
`sieve` (paper-precise second-chance clock). It also has 2 reference
bounds that are not policies:

- `opt`: Belady's MIN (offline, sees the future). No policy can exceed
  it at the same capacity. A live bench rate above `opt` means the live
  run held more than `softcap` entries.
- `noevict`: an unbounded cache (only compulsory misses). It
  corresponds to the live `observe` policy, which is at 100 % there
  because the live bench prewarms every file.

The candidate policies diverge from their BPF counterparts:

| policy | BPF impl | Simulator impl | Why the gap |
|---|---|---|---|
| `lru` | `policy_lru_ops.bpf.c` — approximate age-based FIFO with epoch bumps per admit_replace | strict doubly-linked-list LRU | BPF has no linked-list primitive; age proxy loses ordering info on multi-round eviction cascades |
| `sieve` | `policy_sieve_ops.bpf.c` — per-ino visited bit, cleared during on_evict round | paper hand-pointer clock over linked list | BPF version can only clear per admit_replace round, not per operation |

Result on the bench's own trace (`$OUT/trace.jsonl`, Zipf(0.7)/30/200,
seed 42):

```
                simulator (ms)     live bench (30 s / policy, 2026-09-28)
lru               40.5 %             13.0 %   (older run: 7.5 %)
sieve             46.0 %             65.0 %   (older run: 66.0 %)
opt               61.0 %             —        (65.0 % with best warm start)
```

- `lru` — sim uses strict LRU, bench uses coarse epoch-age proxy and
  evicts every voted entry per round. Sim ≫ bench.
- `sieve` — the live number is at the Belady bound for 8 slots, so it
  is not an 8-slot hit rate. `softcap` only triggers eviction rounds and
  the live admit set stays larger than 8. See the validity review in
  `docs/journal/hit-rate-bench.md`. (An earlier version of this table
  compared against the Go `--zipf` trace, a different sequence, and
  attributed the gap to per-folio `on_hit`.)

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
