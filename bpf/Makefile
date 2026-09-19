CLANG   ?= clang
BPFTOOL ?= bpftool
ARCH    := $(shell uname -m | sed 's/x86_64/x86/;s/aarch64/arm64/')

BPF_CFLAGS := -O2 -g -Wall -target bpf \
              -D__TARGET_ARCH_$(ARCH) \
              -I. -I../include

OBJS := s3fs_fastpath.bpf.o

.PHONY: all clean vmlinux

all: vmlinux $(OBJS)

vmlinux: vmlinux.h

vmlinux.h:
	../scripts/gen-vmlinux.sh > $@

%.bpf.o: %.bpf.c vmlinux.h arena_types.h
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

clean:
	rm -f *.bpf.o
