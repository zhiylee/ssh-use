package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/zhiylee/ssh-use/internal/client"
	"github.com/zhiylee/ssh-use/internal/protocol"
)

var (
	ensureDaemonFn            = client.EnsureDaemon
	connectFn                 = client.Connect
	stdout          io.Writer = os.Stdout
	stderr          io.Writer = os.Stderr
	getwdFn                   = os.Getwd
	getenvFn                  = os.Getenv
	getpidFn                  = os.Getpid
	cancelRequestFn           = func(id string) {
		_, _ = client.Request(context.Background(), protocol.Message{Type: "command.cancel", ID: id})
	}
)

func RunExec(args []string) int {
	host, command, err := parseExecArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 2
	}
	ctx := context.Background()
	if err := ensureDaemonFn(ctx); err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 1
	}
	conn, err := connectFn()
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: connect daemon: %v\n", err)
		return 1
	}
	defer conn.Close()

	var commandID atomic.Value
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		id, _ := commandID.Load().(string)
		if id != "" {
			cancelRequestFn(id)
		}
		_ = conn.Close()
	}()

	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	cwd, _ := getwdFn()
	source := getenvFn("SSH_USE_SOURCE")
	if source == "" {
		source = "unknown"
	}
	if err := enc.Encode(protocol.Message{
		Type:      "exec",
		Host:      host,
		Command:   command,
		Source:    source,
		CWD:       cwd,
		ClientPID: getpidFn(),
	}); err != nil {
		fmt.Fprintf(stderr, "ssh-use: send request: %v\n", err)
		return 1
	}

	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			fmt.Fprintf(stderr, "ssh-use: daemon connection closed: %v\n", err)
			return 1
		}
		if msg.ID != "" {
			commandID.Store(msg.ID)
		}
		switch msg.Type {
		case "stdout_chunk":
			data, err := protocol.DecodeData(msg)
			if err != nil {
				fmt.Fprintf(stderr, "ssh-use: decode stdout chunk: %v\n", err)
				return 1
			}
			_, _ = stdout.Write(data)
		case "stderr_chunk":
			data, err := protocol.DecodeData(msg)
			if err != nil {
				fmt.Fprintf(stderr, "ssh-use: decode stderr chunk: %v\n", err)
				return 1
			}
			_, _ = stderr.Write(data)
		case "command.created":
			commandID.Store(msg.ID)
		case "command.queued":
			if msg.Position > 0 {
				fmt.Fprintf(stderr, "ssh-use: queued on %s position=%d\n", host, msg.Position)
			} else {
				fmt.Fprintf(stderr, "ssh-use: queued on %s\n", host)
			}
		case "approval.waiting":
			fmt.Fprintln(stderr, "ssh-use: waiting for user approval")
			fmt.Fprintln(stderr, "open console: ssh-use tui")
			risk := ""
			if msg.PolicyDecision != nil {
				risk = string(msg.PolicyDecision.Risk)
			}
			fmt.Fprintf(stderr, "id: %s\nhost: %s\nrisk: %s\ncommand: %s\n", msg.ID, host, risk, command)
		case "approval.heartbeat":
			if msg.TUIConnected {
				fmt.Fprintf(stderr, "ssh-use: waiting for approval in TUI id=%s elapsed=%s\n", msg.ID, msg.Elapsed)
			} else {
				fmt.Fprintf(stderr, "ssh-use: still waiting for approval id=%s elapsed=%s open_console=\"ssh-use tui\"\n", msg.ID, msg.Elapsed)
			}
		case "final":
			if msg.OK {
				if msg.RemoteExitCode != nil {
					return *msg.RemoteExitCode
				}
				return 0
			}
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
			}
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		case "error":
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
			}
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		}
	}
}

func parseExecArgs(args []string) (string, string, error) {
	if len(args) < 3 {
		return "", "", fmt.Errorf("usage: ssh-use exec <host> -- <command>")
	}
	host := args[0]
	separator := -1
	for i, arg := range args[1:] {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	if separator == -1 || separator == len(args)-1 {
		return "", "", fmt.Errorf("usage: ssh-use exec <host> -- <command>")
	}
	cmdArgs := args[separator+1:]
	if len(cmdArgs) == 1 {
		return host, cmdArgs[0], nil
	}
	quoted := make([]string, len(cmdArgs))
	for i, arg := range cmdArgs {
		quoted[i] = shellQuote(arg)
	}
	return host, strings.Join(quoted, " "), nil
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	safe := true
	for _, r := range arg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_+-=.,:/", r)) {
			safe = false
			break
		}
	}
	if safe {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func exitCodeForSSHUseError(code string) int {
	switch code {
	case "approval_rejected", "policy_blocked":
		return 126
	case "approval_timeout", "command_timeout":
		return 124
	case "cancelled":
		return 130
	case "connection_failed", "ssh_failed":
		return 255
	case "policy_error":
		return 125
	default:
		return 1
	}
}
