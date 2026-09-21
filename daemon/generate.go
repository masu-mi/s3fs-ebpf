package daemon

// bpf2go compiles bpf/s3fs_fastpath.bpf.c into Go bindings under
// internal/ebpf. The generated files are committed to the repo so `go
// build ./...` works without invoking clang; re-run `go generate ./...`
// (or `make daemon-gen`) after touching bpf/**.

// -mcpu=v3 enables __BPF_FEATURE_ADDR_SPACE_CAST so clang emits
// bpf_addr_space_cast on __arena pointer dereference (Week 2). Requires
// clang 19+; on Ubuntu 24.04 set BPF_CLANG=clang-20.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags "-O2 -g -Wall -mcpu=v3 -I../bpf -I../include" -target bpfel,bpfeb -output-dir internal/ebpf -go-package ebpf s3fsfp ../bpf/s3fs_fastpath.bpf.c
