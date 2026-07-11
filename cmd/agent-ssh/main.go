package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"agent-ssh/internal/cli"
	"agent-ssh/internal/daemon"
	uiterm "agent-ssh/internal/tui"
)

var (
	runExecFn   = cli.RunExec
	runCopyFn   = cli.RunCopy
	runTUIFn    = uiterm.Run
	runDaemonFn = daemon.Run
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "exec":
		return runExecFn(args[1:])
	case "cp":
		return runCopyFn(args[1:])
	case "tui":
		return runTUIFn(args[1:])
	case "daemon":
		if err := runDaemonFn(context.Background()); err != nil {
			fmt.Fprintf(stderr, "agent-ssh daemon: %v\n", err)
			return 1
		}
		return 0
	default:
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  agent-ssh exec <host> -- <command>")
	fmt.Fprintln(w, "  agent-ssh cp [--atomic] <source> <destination>")
	fmt.Fprintln(w, "  agent-ssh tui [--safe]")
}
