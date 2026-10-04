#!/usr/bin/env bash
# kmod-unload.sh [module] [timeout-seconds] — rmmod with retry.
#
# A BPF program that calls the module's kfuncs holds a module reference,
# and after the loading process exits that reference is dropped
# asynchronously (prog free goes through RCU / a workqueue). An rmmod
# issued right after the process exits therefore races it and fails with
# "Module s3fs_kfunc is in use" even though nothing uses it any more.
# Retry for a few seconds; only report failure if it is still held then.
#
# Exit: 0 when the module is unloaded (or was not loaded), 1 otherwise.
set -uo pipefail

mod=${1:-s3fs_kfunc}
timeout=${2:-5}

loaded() { lsmod | grep -q "^${mod} "; }

loaded || exit 0
deadline=$((SECONDS + timeout))
until sudo rmmod "$mod" 2>/dev/null; do
	loaded || exit 0
	if ((SECONDS >= deadline)); then
		echo "rmmod $mod: still in use after ${timeout}s — $(lsmod | grep "^${mod} ")" >&2
		echo "  (a running daemon or mount holds it: 'make unmount', or check 'sudo bpftool prog show')" >&2
		exit 1
	fi
	sleep 0.2
done
