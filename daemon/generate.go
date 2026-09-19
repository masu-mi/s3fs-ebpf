//go:build generate

package daemon

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags "-O2 -g -Wall -I../bpf -I../include" -target bpfel,bpfeb -output-dir internal/ebpf s3fsfp ../bpf/s3fs_fastpath.bpf.c
