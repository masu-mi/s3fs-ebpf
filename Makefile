SHELL := /bin/bash

TOPDIR      := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BIN_DIR     := $(TOPDIR)/bin
KMOD_DIR    := $(TOPDIR)/kmod
BPF_DIR     := $(TOPDIR)/bpf
DAEMON_DIR  := $(TOPDIR)/daemon
DEPLOY_DIR  := $(TOPDIR)/deploy
SCRIPTS_DIR := $(TOPDIR)/scripts

COMPOSE_FILE := $(DEPLOY_DIR)/docker-compose.yml

# Mount lifecycle defaults. MOUNT_POINT matches what scripts/bench-*.sh
# and docs assume. FUSE_MOUNT is only used by the comparison benches.
MOUNT_POINT ?= /mnt/s3demo
FUSE_MOUNT  ?= /mnt/s3fuse
METRICS_ADDR ?= :9600

# Toolchain: CLANG BPFTOOL KDIR KMOD_CC KMOD_PAHOLE KMOD_LDLIB.
# Resolved by scripts/lib/toolchain.sh (shared with `make doctor` and
# scripts/setup-toolchain.sh) so the rules live in one place:
#   CLANG       first clang >= 19 (-mcpu=v3 addr_space_cast; noble's
#               default clang is 18)
#   KMOD_CC     gcc matching the kernel's CONFIG_GCC_VERSION, else the
#               newest gcc >= 14 (Ubuntu mainline headers hard-code gcc-15)
#   KMOD_PAHOLE pahole >= CONFIG_PAHOLE_VERSION, else $$HOME/.local build
#   BPFTOOL     a bpftool that runs (Ubuntu wrapper / Debian /usr/sbin)
# Each is a `?=` default: `make CLANG=clang-21` or an env var wins.
# Background: docs/dev-notes/build-env-dependencies.md
$(foreach v,$(shell $(SCRIPTS_DIR)/detect-toolchain.sh --make),$(eval $(v)))

# LD_LIBRARY_PATH prefix for a $$HOME/.local pahole (needs its libdwarves).
# GO: a go satisfying daemon/go.mod even when an older `go` is first on
# PATH. Its directory is prepended so `go generate`'s `go run` matches,
# and GOROOT is pinned to GO's own root: a GOROOT exported for an older
# install would otherwise make it run the old compiler ("compile: version
# go1.24.5 does not match go tool version go1.25.0").
GO_ENV := $(if $(GO_ROOT),GOROOT=$(GO_ROOT)) $(if $(findstring /,$(GO)),PATH=$(dir $(GO)):$$PATH)

KMOD_ENV := $(if $(KMOD_LDLIB),LD_LIBRARY_PATH=$(KMOD_LDLIB)$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH})
# KMOD_KCFLAGS: -Wno-attributes only when KMOD_CC is older than the kernel's
# gcc (gcc-14 vs mainline's gcc-15 -> counted_by noise); see kmod_kcflags.
KMOD_MAKE_ARGS := KDIR=$(KDIR) CC=$(KMOD_CC) HOSTCC=$(KMOD_CC) PAHOLE=$(KMOD_PAHOLE) \
                  $(if $(KMOD_KCFLAGS),KCFLAGS=$(KMOD_KCFLAGS))

# Preflight: before building, run the doctor checks that gate a component
# (scripts/doctor.sh --only kmod|bpf, or --build for `make all`) so a known
# environment gap fails fast with its fix instead of a compiler error — or
# worse, a "successful" build that only breaks at insmod. The resolved
# toolchain is passed in so command-line overrides are checked too.
# Skip with PREFLIGHT=0.
PREFLIGHT ?= 1
DOCTOR_ENV := CLANG='$(CLANG)' BPFTOOL='$(BPFTOOL)' KDIR='$(KDIR)' \
              KMOD_CC='$(KMOD_CC)' KMOD_PAHOLE='$(KMOD_PAHOLE)'
preflight = $(if $(filter 1,$(PREFLIGHT)),$(DOCTOR_ENV) $(SCRIPTS_DIR)/doctor.sh --quiet $(1),:)

YELLOW := \033[33m
GREEN  := \033[32m
CYAN   := \033[36m
RESET  := \033[0m

.DEFAULT_GOAL := help

CLI_REF_TMPL := $(TOPDIR)/docs/user/cli-reference.md.tmpl
CLI_REF      := $(TOPDIR)/docs/user/cli-reference.md
DAEMON_BIN  := $(BIN_DIR)/s3fs-ebpf

.PHONY: help all build kmod kmod-clean bpf bpf-clean daemon daemon-gen daemon-clean \
        vmlinux vmlinux-clean compose-up compose-down compose-logs \
        mount unmount daemon-run daemon-stop \
        bench bench-write bench-scale bench-3way \
        bench-hitrate bench-hitrate-multi bench-hitrate-mixed setup-bench-env \
        doctor doctor-build toolchain setup-kernel setup-toolchain setup-kbuild \
        lint-scripts cli-reference test-arena test-kunit test-kunit-cov test-daemon test-daemon-cov \
        test-regression test-write-readback test-large-file-read \
        test-evict-readback test-populate-boundary \
        test-pcache-multiread test-pcache-evict-race test-pcache-evict-race-short test-pcache-capacity-tail \
        clean

## Help:
help: ## Show this help
	@printf '\n'
	@printf 'Usage:\n'
	@printf '  ${YELLOW}make${RESET} ${GREEN}<target>${RESET}\n'
	@printf '\n'
	@printf 'Targets:\n'
	@awk 'BEGIN {FS = ":.*?## "} { \
		if (/^[a-zA-Z0-9_-]+:.*?##.*$$/) {printf "    ${YELLOW}%-20s${GREEN}%s${RESET}\n", $$1, $$2} \
		else if (/^## .*$$/) {printf "  ${CYAN}%s${RESET}\n", substr($$1,4)} \
		}' $(MAKEFILE_LIST)

## Build:
# One doctor pass up front reports every gap at once, then the components
# build without re-running their own preflight.
all: ## Build all components (kmod + bpf + daemon)
	@$(call preflight,--build)
	@$(MAKE) --no-print-directory PREFLIGHT=0 kmod bpf daemon
build: all ## Alias of 'all'

# -----------------------------------------------------------------------------
# Kernel module
# -----------------------------------------------------------------------------
# CC / HOSTCC / PAHOLE override the kernel Kbuild defaults, which assume
# the exact gcc / pahole the kernel was built with. See the toolchain
# block at the top of this Makefile.
#
# check-kmod-btf.sh fails the build when kbuild silently skipped module BTF
# (no $$KDIR/vmlinux / old pahole). For external modules kbuild does not
# list vmlinux as a prerequisite of the .ko, so a BTF-less .ko built
# *before* `setup-toolchain.sh kbuild` stays "up to date" afterwards. The
# check removes it, and we relink once: if the environment is fixed (the
# preflight passed) that produces the real BTF; if not, it fails again.
kmod: ## Build kernel module -> kmod/s3fs_kfunc.ko
	@$(call preflight,--only kmod)
	$(KMOD_ENV) $(MAKE) -C $(KMOD_DIR) $(KMOD_MAKE_ARGS)
	@$(SCRIPTS_DIR)/check-kmod-btf.sh --quiet $(KMOD_DIR)/s3fs_kfunc.ko || { \
	  echo "==> s3fs_kfunc.ko has no BTF (stale build from before vmlinux/pahole were fixed?) — relinking once"; \
	  $(KMOD_ENV) $(MAKE) -C $(KMOD_DIR) $(KMOD_MAKE_ARGS) && \
	  $(SCRIPTS_DIR)/check-kmod-btf.sh $(KMOD_DIR)/s3fs_kfunc.ko; }

kmod-clean: ## Clean kernel module build artefacts
	$(MAKE) -C $(KMOD_DIR) $(KMOD_MAKE_ARGS) clean

# -----------------------------------------------------------------------------
# eBPF
# -----------------------------------------------------------------------------
# CLANG / BPFTOOL must be passed explicitly: variables computed in this
# Makefile are not exported, so the sub-make would fall back to plain
# `clang` (18 on noble) and fail on __arena casts.
bpf: ## Build eBPF program -> bpf/s3fs_fastpath.bpf.o
	@$(call preflight,--only bpf)
	@$(MAKE) --no-print-directory $(BPF_DIR)/vmlinux.h
	$(MAKE) -C $(BPF_DIR) CLANG=$(CLANG) BPFTOOL=$(BPFTOOL)

bpf-clean: ## Clean eBPF build artefacts
	$(MAKE) -C $(BPF_DIR) clean

vmlinux: $(BPF_DIR)/vmlinux.h ## Regenerate bpf/vmlinux.h from running kernel BTF

# Generate to a tmp file and only rename on success. Without the
# intermediate, a partial gen-vmlinux.sh run (missing bpftool, no BTF,
# etc.) leaves an empty $@ that make happily accepts on the next build,
# and the bpf sub-make then fails with "unknown type name '__u32'".
$(BPF_DIR)/vmlinux.h:
	@tmp=$$(mktemp $@.tmp.XXXXXX) && \
	  ( BPFTOOL=$(BPFTOOL) $(SCRIPTS_DIR)/gen-vmlinux.sh > $$tmp && chmod 644 $$tmp && mv $$tmp $@ ) || \
	  { rm -f $$tmp; exit 1; }

vmlinux-clean: ## Drop bpf/vmlinux.h
	rm -f $(BPF_DIR)/vmlinux.h

# -----------------------------------------------------------------------------
# Go user process (daemon)
# -----------------------------------------------------------------------------
daemon: | $(BIN_DIR) ## Build Go user process -> bin/s3fs-ebpf
	cd $(DAEMON_DIR) && $(GO_ENV) $(GO) build -o $(BIN_DIR)/s3fs-ebpf ./cmd/s3fs-ebpf

# BPF_CLANG is a command name (clang-20), so bpf2go finds llvm-strip-20.
# The preflight refuses clang < 19: the generated objects are committed and
# embedded, and an old clang silently drops bpf_addr_space_cast (guarded by
# daemon/internal/ebpf/arena_cast_test.go).
daemon-gen: | $(BIN_DIR) ## Run bpf2go codegen then build daemon (needs network)
	@$(call preflight,--only bpf)
	cd $(DAEMON_DIR) && $(GO_ENV) BPF_CLANG=$(CLANG) $(GO) generate ./... && \
		$(GO_ENV) $(GO) build -o $(BIN_DIR)/s3fs-ebpf ./cmd/s3fs-ebpf

daemon-clean: ## Clean Go build cache and bin/s3fs-ebpf
	# Note: intentionally NOT running `go clean ./...` — it removes *.o
	# including bpf2go's embedded s3fsfp_bpf{el,eb}.o (checked in). Use
	# `make daemon-gen` to regenerate those from bpf/**.
	rm -f $(BIN_DIR)/s3fs-ebpf

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

## Observability stack (docker compose):
compose-up: ## Start MinIO + Prometheus + Grafana + node_exporter
	docker compose -f $(COMPOSE_FILE) up -d

compose-down: ## Stop the observability stack
	docker compose -f $(COMPOSE_FILE) down

compose-logs: ## Tail compose logs
	docker compose -f $(COMPOSE_FILE) logs -f

## Runtime lifecycle:
# Convenience targets so operators can bring the stack up manually and
# leave it running while they watch Grafana. bench targets do the same
# lifecycle internally (see scripts/bench-*.sh) so `make bench` from a
# clean host works end-to-end.

mount: kmod ## insmod kmod + mount s3fs_ebpf on $(MOUNT_POINT)
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

unmount: ## umount + rmmod (also stops any running daemon)
	@$(MAKE) --no-print-directory daemon-stop
	@if mountpoint -q $(MOUNT_POINT); then \
		echo "==> umount $(MOUNT_POINT)"; \
		sudo umount $(MOUNT_POINT); \
	fi
	@if lsmod | grep -q '^s3fs_kfunc'; then \
		echo "==> rmmod s3fs_kfunc"; \
		$(SCRIPTS_DIR)/kmod-unload.sh s3fs_kfunc; \
	fi

daemon-run: daemon ## Run daemon in foreground (needs mount + compose-up)
	@if pgrep -f 's3fs-ebpf fastpath' >/dev/null; then \
		echo "==> daemon already running; 'make daemon-stop' first"; exit 1; \
	fi
	@if ! mountpoint -q $(MOUNT_POINT); then \
		echo "==> $(MOUNT_POINT) not mounted; run 'make mount' first"; exit 1; \
	fi
	@echo "==> running $(DAEMON_BIN) fastpath (foreground, Ctrl+C to stop)"
	sudo -E $(DAEMON_BIN) fastpath --s3 --metrics-addr=$(METRICS_ADDR) --sample-every=5s --sync-every=30s

daemon-stop: ## Send SIGINT to any running daemon
	@if pgrep -f 's3fs-ebpf fastpath' >/dev/null; then \
		echo "==> SIGINT s3fs-ebpf fastpath"; \
		sudo pkill -INT -f 's3fs-ebpf fast[p]ath' || true; \
		sleep 1; \
	fi

## Benchmarks:
# All bench targets invoke scripts/ which handle insmod + mount + daemon
# lifecycle internally with trap-based cleanup; make bench from a
# clean host after 'make compose-up; make all' should just work.
bench: all ## Latency bench (hyperfine + microbench + fio) vs s3fs-fuse
	@echo "==> latency bench (hyperfine + microbench + fio) — results in /tmp/bench/"
	$(SCRIPTS_DIR)/bench-latency.sh

bench-write: all ## Write-back throughput bench (dd 1M/10M/100M, ebpf vs fuse)
	@echo "==> write-back throughput bench — results in /tmp/bench-write/"
	$(SCRIPTS_DIR)/bench-write.sh

bench-scale: all ## Parallel scaling bench (numjobs=1..64, stat + read)
	@echo "==> parallel scaling bench — results in /tmp/bench-scale/"
	$(SCRIPTS_DIR)/bench-scale.sh

bench-3way: all ## ebpf vs fuse-raw vs fuse-cache 3-way comparison
	@echo "==> 3-way (ebpf vs fuse-raw vs fuse-cache) bench — results in /tmp/bench-3way/"
	$(SCRIPTS_DIR)/bench-3way.sh

bench-hitrate: all ## Hit-rate bench across Layer 2 policies (Zipf workload, capacity mode)
	@echo "==> hit-rate bench — results in /tmp/bench-hitrate/"
	$(SCRIPTS_DIR)/bench-hitrate.sh

bench-hitrate-multi: all ## Multi-seed / Zipf-α sweep hit-rate bench (set ALPHAS= for sweep)
	@echo "==> multi-seed/alpha hit-rate bench — results in /tmp/bench-hitrate-multi/"
	$(SCRIPTS_DIR)/bench-hitrate-multi.sh

bench-hitrate-mixed: all ## D-01 mixed large+small file hit-rate bench (Y-05 acceptance)
	@echo "==> mixed-file hit-rate bench — results in /tmp/bench-hitrate-mixed/"
	$(SCRIPTS_DIR)/bench-hitrate-mixed.sh

setup-bench-env: ## Install bench dependencies (hyperfine, fio, s3fs, /etc/passwd-s3fs)
	$(SCRIPTS_DIR)/setup-toolchain.sh bench

## Host bootstrap:
doctor: ## Check kernel / headers / compilers / tools and print fixes
	@$(SCRIPTS_DIR)/doctor.sh

doctor-build: ## Same as doctor, limited to what 'make all' needs
	@$(SCRIPTS_DIR)/doctor.sh --build

toolchain: ## Print the resolved CLANG / KMOD_CC / KMOD_PAHOLE / BPFTOOL / KDIR
	@printf '%-12s %s\n' CLANG '$(CLANG)' BPFTOOL '$(BPFTOOL)' KDIR '$(KDIR)' \
	  KMOD_CC '$(KMOD_CC)' KMOD_PAHOLE '$(KMOD_PAHOLE)' KMOD_LDLIB '$(KMOD_LDLIB)' \
	  KMOD_KCFLAGS '$(KMOD_KCFLAGS)' GO '$(GO)' GO_ROOT '$(GO_ROOT)'

setup-kernel: ## Install mainline Linux 6.12 LTS kernel (Ubuntu; reboot after)
	$(SCRIPTS_DIR)/setup-kernel-6.12.sh

setup-toolchain: ## Install everything (clang, bpftool, headers, gcc/pahole, go, bench)
	$(SCRIPTS_DIR)/setup-toolchain.sh

setup-kbuild: ## Re-run kernel-specific fixes (headers, gcc, pahole, vmlinux) after a kernel change
	$(SCRIPTS_DIR)/setup-toolchain.sh headers kbuild

lint-scripts: ## shellcheck scripts/ and s3fs-ebpf.sh
	shellcheck $(SCRIPTS_DIR)/*.sh $(SCRIPTS_DIR)/lib/*.sh $(TOPDIR)/s3fs-ebpf.sh

## Documentation:
cli-reference: $(CLI_REF) ## Regenerate docs/user/cli-reference.md (CLI / make help)

$(CLI_REF): $(CLI_REF_TMPL) $(DAEMON_BIN) Makefile
	@command -v envsubst >/dev/null || { echo "envsubst not found (apt install gettext-base)"; exit 1; }
	@echo "==> regenerating $(notdir $(CLI_REF))"
	@CLI_HELP="$$($(DAEMON_BIN) --help 2>&1)" \
	 CLI_MOUNT_HELP="$$($(DAEMON_BIN) mount --help 2>&1)" \
	 MAKE_HELP="$$($(MAKE) --no-print-directory help | sed 's/\x1b\[[0-9;]*m//g')" \
	 envsubst '$$CLI_HELP $$CLI_MOUNT_HELP $$MAKE_HELP' \
	   < $(CLI_REF_TMPL) > $(CLI_REF)

## Week 1 verification:
test-arena: kmod daemon ## insmod + run bpf_arena/KFunc round-trip probe + rmmod
	@echo "==> insmod kmod/s3fs_kfunc.ko"
	@if ! lsmod | grep -q '^s3fs_kfunc'; then sudo insmod $(KMOD_DIR)/s3fs_kfunc.ko; fi
	@echo "==> running $(DAEMON_BIN) dev test-arena"
	@sudo $(DAEMON_BIN) dev test-arena; rc=$$?; \
	 echo "==> rmmod s3fs_kfunc"; \
	 $(SCRIPTS_DIR)/kmod-unload.sh s3fs_kfunc || true; \
	 exit $$rc

## Unit tests (no daemon, no root):
# KSRC: a Linux >= 6.12 source tree (see docs/dev-notes/kunit.md).
# KUNIT_ARGS: extra kunit.py args, e.g. --arch=x86_64 --qemu_args='-smp 4'.
test-kunit: ## KUnit suites for kmod in UML (KSRC=<linux source tree>)
	@test -n "$(KSRC)" || { echo "set KSRC=<linux source tree>, see docs/dev-notes/kunit.md"; exit 2; }
	KSRC=$(KSRC) $(SCRIPTS_DIR)/kunit.sh $(KUNIT_ARGS)

test-kunit-cov: ## KUnit in UML with clang coverage -> coverage/kmod.lcov (KSRC=...)
	@test -n "$(KSRC)" || { echo "set KSRC=<linux source tree>, see docs/dev-notes/kunit.md"; exit 2; }
	KSRC=$(KSRC) $(SCRIPTS_DIR)/kunit-cov.sh $(CURDIR)/coverage/kmod.lcov $(KUNIT_ARGS)

test-daemon: ## go test for the daemon
	cd $(DAEMON_DIR) && $(GO_ENV) $(GO) test ./...

test-daemon-cov: ## go test with coverage -> coverage/daemon.out
	mkdir -p coverage
	cd $(DAEMON_DIR) && $(GO_ENV) $(GO) test -covermode=atomic -coverprofile=$(CURDIR)/coverage/daemon.out ./...

## Regression tests (T1–T5, AC-01, R1/S1/T3 coverage):
test-regression: test-write-readback test-large-file-read test-evict-readback test-populate-boundary test-pcache-multiread test-pcache-evict-race-short test-pcache-capacity-tail test-pcache-prewarm-none test-arena-partial-readback test-pcache-capacity-noeval test-pcache-capacity-seq test-write-existing test-write-existing-capacity test-write-partial test-fsync-new-file test-pattern-aware-admit test-pick-victim-stale ## Run all regression tests

test-write-readback: kmod daemon ## T1: write → read content match (new file, 4 MiB+, truncate-extend, flush+drop_caches)
	@echo "==> test-write-readback (T1/AC-22)"
	@sudo $(SCRIPTS_DIR)/test-write-readback.sh

test-large-file-read: kmod daemon ## T2/Q3: 300 MiB pcache read, sha256 must match
	@echo "==> test-large-file-read (T2/AC-23)"
	@sudo $(SCRIPTS_DIR)/test-large-file-read.sh

test-evict-readback: kmod daemon ## Q1/S8: evict then re-read — size and content unchanged
	@echo "==> test-evict-readback (Q1/S8/AC-24)"
	@sudo $(SCRIPTS_DIR)/test-evict-readback.sh

test-populate-boundary: kmod daemon ## P2/Q3: POPULATE ioctl boundary values — live ioctl calls (size=0, overflow, >64 MiB, valid)
	@echo "==> test-populate-boundary (P2/Q3/AC-25)"
	@sudo $(SCRIPTS_DIR)/test-populate-boundary.sh

test-pcache-multiread: kmod daemon ## AC-01: pcache split-chunk multi-chunk read sha256 match
	@echo "==> test-pcache-multiread (AC-01)"
	@sudo $(SCRIPTS_DIR)/test-pcache-multiread.sh

test-pcache-evict-race: kmod daemon ## R1/S1/T3: evict-race concurrent stress (default DURATION=120s)
	@echo "==> test-pcache-evict-race (R1/S1/T3)"
	@sudo $(SCRIPTS_DIR)/test-pcache-evict-race.sh

test-pcache-evict-race-short: kmod daemon ## R1/S1/T3: evict-race stress (DURATION=30s, for regression suite)
	@echo "==> test-pcache-evict-race (R1/S1/T3, short, DURATION=30)"
	@sudo DURATION=30 TEST_NAME=test-pcache-evict-race-short $(SCRIPTS_DIR)/test-pcache-evict-race.sh

test-pcache-capacity-tail: kmod daemon ## AC-41: capacity_bytes bounds fd->buf memory (tail-chunk reads)
	@echo "==> test-pcache-capacity-tail (AC-41)"
	@sudo $(SCRIPTS_DIR)/test-pcache-capacity-tail.sh

test-pcache-prewarm-none: kmod daemon ## AC-43: reads work in pcache mode when prewarm admits nothing
	@echo "==> test-pcache-prewarm-none (AC-43)"
	@sudo $(SCRIPTS_DIR)/test-pcache-prewarm-none.sh

test-arena-partial-readback: kmod daemon ## AC-48: arena chunk admits keep i_size (random-first + parallel reads, sha256)
	@echo "==> test-arena-partial-readback (AC-48)"
	@sudo $(SCRIPTS_DIR)/test-arena-partial-readback.sh

test-pcache-capacity-noeval: kmod daemon ## AC-45: capacity_bytes bounds fd->buf without any policy eval (full reads, 4 x 9 MiB)
	@echo "==> test-pcache-capacity-noeval (AC-45)"
	@sudo $(SCRIPTS_DIR)/test-pcache-capacity-noeval.sh

test-pcache-capacity-seq: kmod daemon ## AC-45: a file larger than capacity_bytes read front to back stays within capacity (40 MiB vs 12 MiB)
	@echo "==> test-pcache-capacity-seq (AC-45)"
	@sudo $(SCRIPTS_DIR)/test-pcache-capacity-seq.sh

test-write-existing: kmod daemon ## AC-50: write/truncate/append on an unread existing object keeps the rest (pcache + arena)
	@echo "==> test-write-existing (AC-50)"
	@sudo $(SCRIPTS_DIR)/test-write-existing.sh

test-write-partial: kmod daemon ## AC-51: partial_modify end to end (multipart + server-side copy, 412 fallback)
	@echo "==> test-write-partial (AC-51)"
	@sudo $(SCRIPTS_DIR)/test-write-partial.sh

test-write-existing-capacity: kmod daemon ## AC-50: write/truncate on an existing object bigger than capacity, under read pressure (shed/evict race)
	@echo "==> test-write-existing-capacity (AC-50)"
	@sudo $(SCRIPTS_DIR)/test-write-existing-capacity.sh

test-fsync-new-file: kmod daemon ## a new file written + fsync'ed is not truncated by the create placeholder flush
	@echo "==> test-fsync-new-file"
	@sudo $(SCRIPTS_DIR)/test-fsync-new-file.sh

test-pattern-aware-admit: kmod daemon ## R-01/AC-47: the fentry's access pattern still drives full-file promotion (seq, random, parallel, off)
	@echo "==> test-pattern-aware-admit (AC-47)"
	@sudo $(SCRIPTS_DIR)/test-pattern-aware-admit.sh

test-pick-victim-stale: kmod daemon ## AC-64: an ino that left the admit set does not stay in the policy's own list (pick_victim -> on_remove)
	@echo "==> test-pick-victim-stale (AC-64)"
	@sudo $(SCRIPTS_DIR)/test-pick-victim-stale.sh

test-wtinylfu-harness: ## AC-66: policy_wtinylfu_pick_ops through the verifier, checked against the Go model (no kmod / mount)
	@echo "==> test-wtinylfu-harness (AC-66)"
	@sudo $(SCRIPTS_DIR)/test-wtinylfu-harness.sh

## Cleanup:
clean: kmod-clean bpf-clean daemon-clean vmlinux-clean ## Clean all build artefacts
	rm -rf $(BIN_DIR)
