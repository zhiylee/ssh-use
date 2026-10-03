package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/zhiylee/ssh-use/internal/cli"
	"github.com/zhiylee/ssh-use/internal/daemon"
	"github.com/zhiylee/ssh-use/internal/remote"
	uiterm "github.com/zhiylee/ssh-use/internal/tui"
)

var (
	runExecFn   = cli.RunExec
	runCopyFn   = cli.RunCopy
	runTUIFn    = uiterm.Run
	runDaemonFn = daemon.Run
	runServerFn = cli.RunServer
	runHostsFn  = cli.RunHosts
	runJobsFn   = cli.RunJobs
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	defer remote.CloseClients()
	if len(args) < 1 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "server":
		return runServerFn(args[1:])
	case "hosts", "host":
		return runHostsFn(args[1:])
	case "jobs":
		return runJobsFn(args[1:])
	case "help", "--help", "-h":
		usage(stderr)
		return 0
	case "exec":
		return runExecFn(args[1:])
	case "cp":
		return runCopyFn(args[1:])
	case "tui":
		return runTUIFn(args[1:])
	case "daemon":
		if err := runDaemonFn(context.Background()); err != nil {
			fmt.Fprintf(stderr, "ssh-use daemon: %v\n", err)
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
	fmt.Fprintln(w, "  ssh-use exec [--request-id <id>] <host> -- <command>")
	fmt.Fprintln(w, "  ssh-use cp [--atomic] <source> <destination>")
	fmt.Fprintln(w, "  ssh-use tui [--safe]")
	fmt.Fprintln(w, "  ssh-use hosts <list|get|add|update|delete> [name] [options]")
	fmt.Fprintln(w, "  ssh-use jobs <list|get> [id]")
	fmt.Fprintln(w, "  ssh-use server init --dir <new-directory> --host <hostname>")
	fmt.Fprintln(w, "  ssh-use server --dir <directory> [--listen 127.0.0.1:7443]")
	fmt.Fprintln(w, "  ssh-use server client <add|remove> <name> --dir <directory> [options]")
}
