SHELL := /bin/bash

TOPDIR      := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BIN_DIR     := $(TOPDIR)/bin
KMOD_DIR    := $(TOPDIR)/kmod
BPF_DIR     := $(TOPDIR)/bpf
DAEMON_DIR  := $(TOPDIR)/daemon
DEPLOY_DIR  := $(TOPDIR)/deploy
BENCH_DIR   := $(TOPDIR)/bench
SCRIPTS_DIR := $(TOPDIR)/scripts

COMPOSE_FILE := $(DEPLOY_DIR)/docker-compose.yml

# Mount lifecycle defaults. MOUNT_POINT matches what scripts/bench-*.sh
# and docs assume. FUSE_MOUNT is only used by the comparison benches.
MOUNT_POINT ?= /mnt/s3demo
FUSE_MOUNT  ?= /mnt/s3fuse
METRICS_ADDR ?= :9600

# Week 2 requires __BPF_FEATURE_ADDR_SPACE_CAST (clang 19+ with -mcpu=v3).
# Ubuntu 24.04 ships clang-18.1 as /usr/bin/clang; noble-updates carries
# clang-20 which we prefer when available. Override with `make CLANG=...`.
CLANG ?= $(shell command -v clang-20 2>/dev/null || command -v clang-19 2>/dev/null || echo clang)

# Ubuntu mainline 6.12 kernel headers hard-code CC=gcc-15 but noble only
# ships up to gcc-14. Detect the highest available and pass it through
# to the kmod sub-make. See docs/week1-build-runtime-notes.md #2.
KMOD_CC ?= $(shell command -v gcc-15 2>/dev/null || command -v gcc-14 2>/dev/null || command -v gcc 2>/dev/null || echo gcc)

# Distro pahole 1.25 silently emits empty BTF against kernels built with
# pahole 1.30+; a user-local rebuild lives at $HOME/.local. See Week 1
# notes #3. Fall back to system pahole if the local build isn't there.
KMOD_PAHOLE ?= $(shell test -x $(HOME)/.local/bin/pahole && echo $(HOME)/.local/bin/pahole || command -v pahole 2>/dev/null || echo pahole)

# Locally-built pahole needs its libdwarves; prepend if present.
KMOD_LDLIB := $(HOME)/.local/lib

.DEFAULT_GOAL := help

README_TMPL := $(TOPDIR)/README.md.tmpl
README      := $(TOPDIR)/README.md
DAEMON_BIN  := $(BIN_DIR)/s3fs-ebpf

.PHONY: help all build kmod kmod-clean bpf bpf-clean daemon daemon-gen daemon-clean \
        vmlinux vmlinux-clean compose-up compose-down compose-logs \
        mount unmount daemon-run daemon-stop \
        bench bench-write bench-scale bench-3way bench-dry \
        setup-kernel setup-toolchain readme test-arena clean

## help: Show this help (default target)
help:
	@echo "s3fs-ebpf — targets"
	@echo ""
	@echo "Build (individual components):"
	@echo "  make kmod           Build kernel module   -> kmod/s3fs_kfunc.ko"
	@echo "  make bpf            Build eBPF program    -> bpf/s3fs_fastpath.bpf.o"
	@echo "  make daemon         Build Go user process -> bin/s3fs-ebpf"
	@echo ""
	@echo "Build (combined / codegen):"
	@echo "  make all            kmod + bpf + daemon"
	@echo "  make build          alias of 'all'"
	@echo "  make daemon-gen     bpf2go codegen then daemon (needs network)"
	@echo ""
	@echo "Housekeeping:"
	@echo "  make vmlinux        Regenerate bpf/vmlinux.h from running kernel BTF"
	@echo "  make vmlinux-clean  Drop bpf/vmlinux.h"
	@echo "  make kmod-clean     Clean kernel module build artefacts"
	@echo "  make bpf-clean      Clean eBPF build artefacts"
	@echo "  make daemon-clean   Clean Go build cache and bin/s3fs-ebpf"
	@echo "  make clean          Clean everything"
	@echo ""
	@echo "Observability stack (docker compose):"
	@echo "  make compose-up     Start MinIO + Prometheus + Grafana + node_exporter"
	@echo "  make compose-down   Stop the stack"
	@echo "  make compose-logs   Tail compose logs"
	@echo ""
	@echo "Runtime lifecycle (kmod + mount + daemon):"
	@echo "  make mount          insmod kmod + mount -t s3fs_ebpf on $(MOUNT_POINT)"
	@echo "  make unmount        umount + rmmod (also stops any running daemon)"
	@echo "  make daemon-run     Foreground: run bin/s3fs-ebpf fastpath (needs mount + compose-up)"
	@echo "  make daemon-stop    SIGINT any running daemon"
	@echo ""
	@echo "Benchmarks (all auto-start compose-up requirement + kmod/mount/daemon in-script):"
	@echo "  make bench          Latency bench (hyperfine + microbench + fio) vs s3fs-fuse"
	@echo "  make bench-write    Write-back throughput bench (dd 1M/10M/100M, ebpf vs fuse)"
	@echo "  make bench-scale    Parallel scaling (numjobs=1..64) stat + read"
	@echo "  make bench-3way     ebpf vs fuse-raw vs fuse-cache 3-way comparison"
	@echo "  make bench-dry      Legacy runner.sh dry-run"
	@echo ""
	@echo "Week 1 verification (requires 6.12+ kernel):"
	@echo "  make test-arena     insmod + run bpf_arena/KFunc round-trip probe + rmmod"
	@echo ""
	@echo "Environment bootstrap (run once on a fresh host):"
	@echo "  make setup-kernel     Install mainline Linux 6.12 LTS kernel (reboot after)"
	@echo "  make setup-toolchain  Install clang, bpftool, go, docker, fio, bcc"
	@echo ""
	@echo "Documentation:"
	@echo "  make readme         Regenerate README.md from README.md.tmpl (envsubst)"
	@echo ""
	@echo "Tip: 'make <target> -n' shows the recipe without executing it."

## all / build: build every component
all: kmod bpf daemon
build: all

# -----------------------------------------------------------------------------
# Kernel module
# -----------------------------------------------------------------------------
# LD_LIBRARY_PATH lets a $HOME/.local pahole find its libdwarves.so;
# CC / HOSTCC / PAHOLE override the kernel Kbuild defaults that assume
# gcc-15 / distro pahole. All three are computed at the top of this
# Makefile and can be overridden per-invocation.
kmod:
	LD_LIBRARY_PATH=$(KMOD_LDLIB):$$LD_LIBRARY_PATH \
	$(MAKE) -C $(KMOD_DIR) CC=$(KMOD_CC) HOSTCC=$(KMOD_CC) PAHOLE=$(KMOD_PAHOLE)

kmod-clean:
	$(MAKE) -C $(KMOD_DIR) CC=$(KMOD_CC) HOSTCC=$(KMOD_CC) clean

# -----------------------------------------------------------------------------
# eBPF
# -----------------------------------------------------------------------------
bpf: vmlinux
	$(MAKE) -C $(BPF_DIR)

bpf-clean:
	$(MAKE) -C $(BPF_DIR) clean

vmlinux: $(BPF_DIR)/vmlinux.h

# Generate to a tmp file and only rename on success. Without the
# intermediate, a partial gen-vmlinux.sh run (missing bpftool, no BTF,
# etc.) leaves an empty $@ that make happily accepts on the next build,
# and the bpf sub-make then fails with "unknown type name '__u32'".
$(BPF_DIR)/vmlinux.h:
	@tmp=$$(mktemp $@.tmp.XXXXXX) && \
	  ( $(SCRIPTS_DIR)/gen-vmlinux.sh > $$tmp && mv $$tmp $@ ) || \
	  { rm -f $$tmp; exit 1; }

vmlinux-clean:
	rm -f $(BPF_DIR)/vmlinux.h

# -----------------------------------------------------------------------------
# Go user process (daemon)
# -----------------------------------------------------------------------------
daemon: | $(BIN_DIR)
	cd $(DAEMON_DIR) && go build -o $(BIN_DIR)/s3fs-ebpf ./cmd/s3fs-ebpf

daemon-gen: | $(BIN_DIR)
	cd $(DAEMON_DIR) && BPF_CLANG=$(CLANG) go generate ./... && \
		go build -o $(BIN_DIR)/s3fs-ebpf ./cmd/s3fs-ebpf

daemon-clean:
	# Note: intentionally NOT running `go clean ./...` — it removes *.o
	# including bpf2go's embedded s3fsfp_bpf{el,eb}.o (checked in). Use
	# `make daemon-gen` to regenerate those from bpf/**.
	rm -f $(BIN_DIR)/s3fs-ebpf

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# -----------------------------------------------------------------------------
# docker-compose (observability stack)
# -----------------------------------------------------------------------------
compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

compose-down:
	docker compose -f $(COMPOSE_FILE) down

compose-logs:
	docker compose -f $(COMPOSE_FILE) logs -f

# -----------------------------------------------------------------------------
# Runtime lifecycle (kmod + mount + daemon)
# -----------------------------------------------------------------------------
# Convenience targets so operators can bring the stack up manually and
# leave it running while they watch Grafana. bench targets do the same
# lifecycle internally (see scripts/bench-*.sh) so `make bench` from a
# clean host works end-to-end.

mount: kmod
	@if ! lsmod | grep -q '^s3fs_kfunc'; then \
		echo "==> insmod $(KMOD_DIR)/s3fs_kfunc.ko"; \
		sudo insmod $(KMOD_DIR)/s3fs_kfunc.ko; \
	fi
	@if ! mountpoint -q $(MOUNT_POINT); then \
		echo "==> mount -t s3fs_ebpf none $(MOUNT_POINT)"; \
		sudo mkdir -p $(MOUNT_POINT); \
		sudo mount -t s3fs_ebpf none $(MOUNT_POINT); \
	fi
	@echo "==> ready: $(MOUNT_POINT) (start daemon with 'make daemon-run')"

unmount:
	@$(MAKE) --no-print-directory daemon-stop
	@if mountpoint -q $(MOUNT_POINT); then \
		echo "==> umount $(MOUNT_POINT)"; \
		sudo umount $(MOUNT_POINT); \
	fi
	@if lsmod | grep -q '^s3fs_kfunc'; then \
		echo "==> rmmod s3fs_kfunc"; \
		sudo rmmod s3fs_kfunc; \
	fi

daemon-run: daemon
	@if pgrep -f 's3fs-ebpf fastpath' >/dev/null; then \
		echo "==> daemon already running; 'make daemon-stop' first"; exit 1; \
	fi
	@if ! mountpoint -q $(MOUNT_POINT); then \
		echo "==> $(MOUNT_POINT) not mounted; run 'make mount' first"; exit 1; \
	fi
	@echo "==> running $(DAEMON_BIN) fastpath (foreground, Ctrl+C to stop)"
	LD_LIBRARY_PATH=$(KMOD_LDLIB):$$LD_LIBRARY_PATH sudo -E \
		$(DAEMON_BIN) fastpath --s3 --metrics-addr=$(METRICS_ADDR) --sample-every=5s --sync-every=30s

daemon-stop:
	@if pgrep -f 's3fs-ebpf fastpath' >/dev/null; then \
		echo "==> SIGINT s3fs-ebpf fastpath"; \
		sudo pkill -INT -f 's3fs-ebpf fastpath' || true; \
		sleep 1; \
	fi

# -----------------------------------------------------------------------------
# Benchmarks (all invoke scripts/ which handle insmod + mount + daemon
# lifecycle internally with trap-based cleanup; make bench from a
# clean host after 'make compose-up; make all' should just work).
# -----------------------------------------------------------------------------
bench: all
	@echo "==> latency bench (hyperfine + microbench + fio) — results in /tmp/bench/"
	$(SCRIPTS_DIR)/bench-latency.sh

bench-write: all
	@echo "==> write-back throughput bench — results in /tmp/bench-write/"
	$(SCRIPTS_DIR)/bench-write.sh

bench-scale: all
	@echo "==> parallel scaling bench — results in /tmp/bench-scale/"
	$(SCRIPTS_DIR)/bench-scale.sh

bench-3way: all
	@echo "==> 3-way (ebpf vs fuse-raw vs fuse-cache) bench — results in /tmp/bench-3way/"
	$(SCRIPTS_DIR)/bench-3way.sh

# Legacy runner (Week 0/1 skeleton). Kept for the --dry-run flow;
# scripts/bench-latency.sh replaces the actual bench execution.
bench-dry:
	$(BENCH_DIR)/runner.sh --dry-run

# -----------------------------------------------------------------------------
# Host bootstrap
# -----------------------------------------------------------------------------
setup-kernel:
	$(SCRIPTS_DIR)/setup-kernel-6.12.sh

setup-toolchain:
	$(SCRIPTS_DIR)/setup-toolchain.sh

# -----------------------------------------------------------------------------
# Documentation (envsubst-driven)
# -----------------------------------------------------------------------------
# Regenerates README.md from README.md.tmpl by injecting live CLI/help output.
# Only the placeholders listed after envsubst are substituted; other `$VAR`
# tokens in the template (should any appear) are left verbatim.
readme: $(README)

$(README): $(README_TMPL) $(DAEMON_BIN) Makefile
	@command -v envsubst >/dev/null || { echo "envsubst not found (apt install gettext-base)"; exit 1; }
	@echo "==> regenerating $(notdir $(README))"
	@CLI_HELP="$$($(DAEMON_BIN) --help 2>&1)" \
	 CLI_MOUNT_HELP="$$($(DAEMON_BIN) mount --help 2>&1)" \
	 MAKE_HELP="$$($(MAKE) --no-print-directory help)" \
	 envsubst '$$CLI_HELP $$CLI_MOUNT_HELP $$MAKE_HELP' \
	   < $(README_TMPL) > $(README)

# -----------------------------------------------------------------------------
# Week 1: arena / KFunc round-trip probe
# -----------------------------------------------------------------------------
# Loads the kmod, runs the daemon's `test-arena` subcommand, then unloads.
# Requires a 6.12+ kernel; on older kernels arena.New() will fail with
# a clear message and the target exits non-zero.
test-arena: kmod daemon
	@echo "==> insmod kmod/s3fs_kfunc.ko"
	@if ! lsmod | grep -q '^s3fs_kfunc'; then sudo insmod $(KMOD_DIR)/s3fs_kfunc.ko; fi
	@echo "==> running $(DAEMON_BIN) test-arena"
	@sudo $(DAEMON_BIN) test-arena; rc=$$?; \
	 echo "==> rmmod s3fs_kfunc"; \
	 sudo rmmod s3fs_kfunc || true; \
	 exit $$rc

# -----------------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------------
clean: kmod-clean bpf-clean daemon-clean vmlinux-clean
	rm -rf $(BIN_DIR)
