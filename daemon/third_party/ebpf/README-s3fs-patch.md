# cilium/ebpf v0.22.0 — s3fs-ebpf local patch

Upstream: <https://github.com/cilium/ebpf> @ v0.22.0 (2026-06-26)

This tree is a byte-for-byte copy of `cilium/ebpf@v0.22.0` with a single
patch applied:

## Patch

`asm/instruction.go` — `Instruction.Marshal`

```go
// upstream (BUG)
ins.Offset = newOffset

// patched
if newOffset != 0 {
    ins.Offset = newOffset
}
```

## Why

Clang emits `bpf_addr_space_cast(dst, src, from_as, to_as)` for any
dereference of an `__attribute__((address_space(1)))` pointer. On the
wire the instruction is `BPF_MOV | BPF_ALU64 | BPF_X` (opcode `0xbf`)
with `Offset = 1` (== `BPF_ADDR_SPACE_CAST`) and `imm = (from << 16) | to`.

Upstream `Marshal` unconditionally overwrites `ins.Offset = newOffset`,
where `newOffset` defaults to 0 unless the ALU op is one of `SDiv`,
`SMod`, `MovSX8/16/32`. Plain `Mov` falls through with `newOffset = 0`,
so any caller-set `Offset` (including the `addr_space_cast` marker) is
silently zeroed on the wire.

The verifier then sees `MOV|ALU64|X` with `off=0` and `imm=1`, matches
neither the addr_space_cast branch nor the plain-MOV shape, and rejects
with `"BPF_MOV uses reserved fields"`.

Repro without this patch: build the Week 2 arena probe in `bpf/` and run
`make test-arena` on a 6.9+ kernel. Verifier log shows

```
10: (bf) r2 = r3
BPF_MOV uses reserved fields
```

instead of the expected `r2 = addr_space_cast(r3, 0x0, 0x1)`.

## Migration path off this vendor tree

1. Fork `cilium/ebpf` on GitHub, apply the same one-liner, tag e.g.
   `v0.22.0-arena-fix`, and open an upstream PR.
2. In `daemon/go.mod`, change the `replace` target from
   `./third_party/ebpf` to the fork tag.
3. Delete `daemon/third_party/ebpf/` and `go mod tidy`.
4. When upstream merges, drop the `replace` and bump to the next
   released tag.
