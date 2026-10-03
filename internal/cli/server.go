package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/zhiylee/ssh-use/internal/daemon"
	"github.com/zhiylee/ssh-use/internal/paths"
	"github.com/zhiylee/ssh-use/internal/remote"
)

func RunServer(args []string) int {
	if len(args) > 0 && args[0] == "client" {
		return runServerClient(args[1:])
	}
	initMode := len(args) > 0 && args[0] == "init"
	if initMode {
		args = args[1:]
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "TLS credentials directory")
	host := fs.String("host", "localhost", "certificate hostname / client endpoint (init only)")
	port := fs.String("port", "7443", "client endpoint port (init only)")
	listen := fs.String("listen", "127.0.0.1:7443", "TLS listen address")
	cert := fs.String("tls-cert", "", "server TLS certificate")
	key := fs.String("tls-key", "", "server TLS private key")
	auth := fs.String("auth-file", "", "client token hashes and roles")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	*dir = paths.ExpandHome(*dir)
	if initMode {
		if *dir == "" {
			fmt.Fprintln(stderr, "ssh-use: server init requires --dir (a new directory)")
			return 2
		}
		if err := remote.Init(*dir, *host, *port); err != nil {
			fmt.Fprintf(stderr, "ssh-use: initialize server: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Created %s with TLS credentials, auth.yaml, and admin/agent client bundles.\n", *dir)
		return 0
	}
	if *dir != "" {
		if *cert == "" {
			*cert = filepath.Join(*dir, "tls.crt")
		}
		if *key == "" {
			*key = filepath.Join(*dir, "tls.key")
		}
		if *auth == "" {
			*auth = filepath.Join(*dir, "auth.yaml")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stderr, "ssh-use: serving gRPC/TLS on %s; SSH configuration: %s\n", *listen, paths.ConfigPath())
	if err := daemon.RunRemote(ctx, daemon.RemoteOptions{Listen: *listen, CertFile: paths.ExpandHome(*cert), KeyFile: paths.ExpandHome(*key), AuthFile: paths.ExpandHome(*auth)}); err != nil {
		fmt.Fprintf(stderr, "ssh-use server: %v\n", err)
		return 1
	}
	return 0
}

func runServerClient(args []string) int {
	if len(args) < 2 || (args[0] != "add" && args[0] != "remove") {
		fmt.Fprintln(stderr, "usage: ssh-use server client <add|remove> <name> --dir <server-dir> [--role agent|admin --out <new-dir> --endpoint host:port]")
		return 2
	}
	fs := flag.NewFlagSet("server client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "server credentials directory")
	role := fs.String("role", "agent", "admin or agent")
	out := fs.String("out", "", "new client bundle directory")
	endpoint := fs.String("endpoint", "", "server hostname:port matching TLS certificate")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || *dir == "" {
		return 2
	}
	if err := remote.EditClient(paths.ExpandHome(*dir), args[1], *role, paths.ExpandHome(*out), *endpoint, args[0] == "remove"); err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s client: %s\n", args[0], args[1])
	return 0
}
