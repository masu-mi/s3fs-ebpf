CLANG   ?= clang
BPFTOOL ?= bpftool
ARCH    := $(shell uname -m | sed 's/x86_64/x86/;s/aarch64/arm64/')

# -mcpu=v3 turns on __BPF_FEATURE_ADDR_SPACE_CAST so clang emits
# bpf_addr_space_cast when dereferencing __arena pointers (Week 2).
# Requires clang 19+; on Ubuntu 24.04 set CLANG=clang-20.
BPF_CFLAGS := -O2 -g -Wall -target bpf -mcpu=v3 \
              -D__TARGET_ARCH_$(ARCH) \
              -I. -I../include

OBJS := s3fs_fastpath.bpf.o policy_observe.bpf.o \
        policy_freq2bit.bpf.o policy_freq2bit_ops.bpf.o \
        policy_lru_ops.bpf.o policy_sieve_ops.bpf.o \
        policy_s3fifo_ops.bpf.o \
        policy_wtinylfu_ops.bpf.o policy_wtinylfu_full_ops.bpf.o \
        policy_pressure_ops.bpf.o policy_time_decay_ops.bpf.o \
        policy_size_aware_ops.bpf.o \
        kfunc_walk.bpf.o

.PHONY: all clean vmlinux check-clang

all: vmlinux $(OBJS)

# clang < 19 does not define __BPF_FEATURE_ADDR_SPACE_CAST, and
# arena_types.h then *silently* falls back to plain pointers — the build
# succeeds but the objects differ. Fail loudly instead.
check-clang:
	@v=$$($(CLANG) --version 2>/dev/null | sed -nE 's/.*clang version ([0-9]+).*/\1/p' | head -n1); \
	if [ -z "$$v" ] || [ "$$v" -lt 19 ]; then \
		echo "ERROR: CLANG=$(CLANG) is clang $${v:-?}; need >= 19 for -mcpu=v3 __arena casts." >&2; \
		echo "       Run 'make doctor' (C-1) or pass CLANG=clang-20." >&2; \
		exit 1; \
	fi

vmlinux: vmlinux.h

vmlinux.h:
	BPFTOOL=$(BPFTOOL) ../scripts/gen-vmlinux.sh > $@

%.bpf.o: %.bpf.c vmlinux.h arena_types.h | check-clang
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

clean:
	rm -f *.bpf.o
