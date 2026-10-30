package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// run executes the command tree and returns the exit code with the group
// usage output.
func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		var ee exitError
		if !errors.As(err, &ee) {
			return -1, out.String(), err.Error()
		}
		code = ee.code
	}
	return code, out.String(), errOut.String()
}

func TestGroupUsage(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		{"no args", nil, 2, "", "Usage: s3fs-ebpf <command>"},
		{"help", []string{"help"}, 0, "Usage: s3fs-ebpf <command>", ""},
		{"-h", []string{"-h"}, 0, "Usage: s3fs-ebpf <command>", ""},
		{"--help", []string{"--help"}, 0, "Usage: s3fs-ebpf <command>", ""},
		{"help with arg", []string{"help", "dev"}, 0, "Usage: s3fs-ebpf <command>", ""},
		{"unknown", []string{"bogus"}, 2, "", "unknown command: bogus"},
		{"dev no args", []string{"dev"}, 2, "", "Usage: s3fs-ebpf dev <command>"},
		{"dev help", []string{"dev", "help"}, 0, "Usage: s3fs-ebpf dev <command>", ""},
		{"dev -h", []string{"dev", "-h"}, 0, "Usage: s3fs-ebpf dev <command>", ""},
		{"dev unknown", []string{"dev", "bogus"}, 2, "", "unknown dev command: bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := run(tt.args...)
			if code != tt.code {
				t.Errorf("exit code = %d, want %d", code, tt.code)
			}
			if !strings.Contains(out, tt.inStdout) || (tt.inStdout == "" && out != "") {
				t.Errorf("stdout = %q, want it to contain %q", out, tt.inStdout)
			}
			if !strings.Contains(errOut, tt.inStderr) || (tt.inStderr == "" && errOut != "") {
				t.Errorf("stderr = %q, want it to contain %q", errOut, tt.inStderr)
			}
		})
	}
}

// Every command in the usage text must be registered, and "umount" must stay
// an alias of "unmount".
func TestCommandsRegistered(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"mount", "unmount", "umount", "fastpath", "stats", "policy", "hash-policy", "s3-list", "s3-stat", "dev"} {
		if c, _, err := root.Find([]string{name}); err != nil || c == root {
			t.Errorf("command %q is not registered (err=%v)", name, err)
		}
	}
	for _, name := range []string{"test-arena", "verifier-demo", "probe-kvbase", "microbench", "trace-replay", "ann-sim"} {
		if c, _, err := root.Find([]string{"dev", name}); err != nil || c.Name() != name {
			t.Errorf("dev command %q is not registered (err=%v)", name, err)
		}
	}
}

// A subcommand's exit code reaches the caller as an exitError; 0 is nil.
func TestExitCodePropagates(t *testing.T) {
	if code, _, _ := run("stats"); code != 0 {
		t.Errorf("stats exit = %d, want 0", code)
	}
	if code, _, _ := run("hash-policy"); code != 2 {
		t.Errorf("hash-policy without args exit = %d, want 2", code)
	}
}
