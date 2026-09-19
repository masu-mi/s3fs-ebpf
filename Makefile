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

CLANG ?= clang

.DEFAULT_GOAL := help

README_TMPL := $(TOPDIR)/README.md.tmpl
README      := $(TOPDIR)/README.md
DAEMON_BIN  := $(BIN_DIR)/s3fs-ebpf

.PHONY: help all build kmod kmod-clean bpf bpf-clean daemon daemon-gen daemon-clean \
        vmlinux vmlinux-clean compose-up compose-down compose-logs bench bench-dry \
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
	@echo "Benchmarks:"
	@echo "  make bench          Run fio + BCC jobs (requires mounted target)"
	@echo "  make bench-dry      Verify job files without running (dry-run)"
	@echo ""
	@echo "Week 1 verification (requires 6.9+ kernel):"
	@echo "  make test-arena     insmod + run bpf_arena/KFunc round-trip probe + rmmod"
	@echo ""
	@echo "Environment bootstrap (run once on a fresh host):"
	@echo "  make setup-kernel     Install mainline Linux 6.9 kernel (reboot after)"
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
kmod:
	$(MAKE) -C $(KMOD_DIR)

kmod-clean:
	$(MAKE) -C $(KMOD_DIR) clean

# -----------------------------------------------------------------------------
# eBPF
# -----------------------------------------------------------------------------
bpf: vmlinux
	$(MAKE) -C $(BPF_DIR)

bpf-clean:
	$(MAKE) -C $(BPF_DIR) clean

vmlinux: $(BPF_DIR)/vmlinux.h

$(BPF_DIR)/vmlinux.h:
	$(SCRIPTS_DIR)/gen-vmlinux.sh > $@

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
# Benchmarks
# -----------------------------------------------------------------------------
bench:
	$(BENCH_DIR)/runner.sh

bench-dry:
	$(BENCH_DIR)/runner.sh --dry-run

# -----------------------------------------------------------------------------
# Host bootstrap
# -----------------------------------------------------------------------------
setup-kernel:
	$(SCRIPTS_DIR)/setup-kernel-6.9.sh

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
# Requires a 6.9+ kernel; on older kernels arena.New() will fail with
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
