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

	"github.com/google/uuid"
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
	requestID := uuid.NewString()
	if len(args) > 0 && args[0] == "--request-id" {
		if len(args) < 3 || len(args[1]) == 0 || len(args[1]) > 128 {
			fmt.Fprintln(stderr, "ssh-use: --request-id requires 1-128 characters")
			return 2
		}
		requestID, args = args[1], args[2:]
	}
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
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigCh:
		case <-done:
			return
		}
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
		RequestID: requestID,
		Host:      host,
		Command:   command,
		Source:    source,
		CWD:       cwd,
		ClientPID: getpidFn(),
	}); err != nil {
		fmt.Fprintf(stderr, "ssh-use: send request: %v\n", err)
		if client.IsRemote() {
			fmt.Fprintf(stderr, "ssh-use: request ID: %s; retry only with --request-id %s to avoid duplicate execution\n", requestID, requestID)
		}
		return 1
	}

	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			fmt.Fprintf(stderr, "ssh-use: daemon connection closed: %v\n", err)
			if id, _ := commandID.Load().(string); id != "" {
				fmt.Fprintf(stderr, "ssh-use: inspect status with ssh-use jobs get %s\n", id)
			}
			if client.IsRemote() {
				fmt.Fprintf(stderr, "ssh-use: request ID: %s; retry only with --request-id %s to avoid duplicate execution\n", requestID, requestID)
			}
			return 1
		}
		if msg.ID != "" {
			commandID.Store(msg.ID)
		}
		switch msg.Type {
		case "command":
			fmt.Fprintf(stderr, "ssh-use: request already accepted as %s; inspect with ssh-use jobs get %s\n", msg.ID, msg.ID)
			if msg.Record != nil && msg.Record.SSHUseErrorCode != "" {
				writeResultDiagnostic(msg.ID, 0, msg.Record.SSHUseErrorCode)
				return exitCodeForSSHUseError(msg.Record.SSHUseErrorCode)
			}
			if msg.Record != nil && msg.Record.RemoteExitCode != nil {
				writeResultDiagnostic(msg.ID, *msg.Record.RemoteExitCode, "")
				return *msg.Record.RemoteExitCode
			}
			return 1
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
					writeResultDiagnostic(msg.ID, *msg.RemoteExitCode, "")
					return *msg.RemoteExitCode
				}
				return 0
			}
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
			}
			writeResultDiagnostic(msg.ID, 0, msg.SSHUseErrorCode)
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		case "error":
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
			}
			writeResultDiagnostic(msg.ID, 0, msg.SSHUseErrorCode)
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		}
	}
}

// Exit numbers overlap with remote program exits. Emit the origin separately
// without adding status text to stdout or changing the exit-code contract.
func writeResultDiagnostic(id string, remoteExit int, serviceCode string) {
	if serviceCode != "" {
		fmt.Fprintf(stderr, "ssh-use: ssh_use_error_code=%s job_id=%s\n", serviceCode, id)
	} else if remoteExit != 0 {
		fmt.Fprintf(stderr, "ssh-use: remote_exit_code=%d job_id=%s\n", remoteExit, id)
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
