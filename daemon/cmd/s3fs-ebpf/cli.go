package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// exitError carries a subcommand's exit code out of cobra's RunE so that
// main can pass it to os.Exit unchanged.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// passthrough wraps a runXxx(args) int function as a cobra command.  Flag
// parsing stays with the function's own flag.FlagSet (DisableFlagParsing), so
// every existing invocation, including -flag and --flag, -h and the exit
// codes, behaves as before.
func passthrough(use, short string, run func([]string) int, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		Aliases:            aliases,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(_ *cobra.Command, args []string) error {
			if code := run(args); code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}

// groupRun is the RunE of a command group (root, dev): it prints the usage
// text for "help" / "-h" / "--help" and rejects anything else with exit 2.
func groupRun(name, usageText string, out, errOut io.Writer) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == 0 {
			fmt.Fprint(errOut, usageText)
			return exitError{2}
		}
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(out, usageText)
			return nil
		}
		fmt.Fprintf(errOut, "unknown %s: %s\n\n%s", name, args[0], usageText)
		return exitError{2}
	}
}

// newRootCmd builds the command tree.  out / errOut receive the group usage
// text; the subcommands write to os.Stdout / os.Stderr themselves.
func newRootCmd(out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:                "s3fs-ebpf",
		Short:              "s3fs on eBPF/kmod fast path",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE:               groupRun("command", usage, out, errOut),
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetHelpCommand(&cobra.Command{
		Use:    "help",
		Hidden: true,
		Run:    func(*cobra.Command, []string) { fmt.Fprint(out, usage) },
	})

	dev := &cobra.Command{
		Use:                "dev",
		Short:              "Development / probe / demo commands",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE:               groupRun("dev command", devUsage, out, errOut),
	}
	dev.AddCommand(
		passthrough("test-arena", "bpf_arena + KFunc round-trip probe", runTestArena),
		passthrough("verifier-demo", "verifier rejection demo", runVerifierDemo),
		passthrough("probe-kvbase", "arena ioctl kvbase probe", runProbeKvbase),
		passthrough("microbench", "tight-loop stat/read latency bench", runMicrobench),
		passthrough("trace-replay", "offline policy hit-rate simulator", runTraceReplay),
		passthrough("ann-sim", "synthetic ANN (IVF/HNSW) chunk-cache policy comparison", runAnnSim),
	)

	root.AddCommand(
		passthrough("mount", "Mount an S3 bucket via the s3fs_ebpf VFS", runMount),
		passthrough("unmount", "Unmount a previously mounted target", runUnmount, "umount"),
		passthrough("fastpath", "Run the fastpath daemon", runFastpath),
		passthrough("stats", "Print fast/slow path counters", runStats),
		passthrough("policy", "Hot-reload the cache policy blob", runPolicy),
		passthrough("hash-policy", "Print SHA256 for a user policy .bpf.o", runHashPolicy),
		passthrough("s3-list", "List objects in a bucket", runS3List),
		passthrough("s3-stat", "Stat a single object", runS3Stat),
		dev,
	)
	return root
}
