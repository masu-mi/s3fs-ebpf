package daemon

// bpf2go compiles bpf/s3fs_fastpath.bpf.c into Go bindings under
// internal/ebpf. The generated files are committed to the repo so `go
// build ./...` works without invoking clang; re-run `go generate ./...`
// (or `make daemon-gen`) after touching bpf/**.

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags "-O2 -g -Wall -I../bpf -I../include" -target bpfel,bpfeb -output-dir internal/ebpf -go-package ebpf s3fsfp ../bpf/s3fs_fastpath.bpf.c
