package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"agent-ssh/internal/model"
	"agent-ssh/internal/protocol"
)

func TestParseExecArgsSingleString(t *testing.T) {
	host, cmd, err := parseExecArgs([]string{"prod1", "--", "cd /app && git status"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "prod1" || cmd != "cd /app && git status" {
		t.Fatalf("host=%q cmd=%q", host, cmd)
	}
}

func TestParseExecArgsMultipleArgs(t *testing.T) {
	_, cmd, err := parseExecArgs([]string{"prod1", "--", "docker", "logs", "--tail=100", "api service"})
	if err != nil {
		t.Fatal(err)
	}
	want := "docker logs --tail=100 'api service'"
	if cmd != want {
		t.Fatalf("cmd=%q want=%q", cmd, want)
	}
}

func TestParseExecArgsErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"host"}, {"host", "--"}, {"host", "cmd"}} {
		if _, _, err := parseExecArgs(args); err == nil {
			t.Fatalf("expected error for %#v", args)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"abc":       "abc",
		"":          "''",
		"two words": "'two words'",
		"it's":      "'it'\\''s'",
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestExitCodeForAgentSSHError(t *testing.T) {
	tests := map[string]int{
		"approval_rejected": 126,
		"policy_blocked":    126,
		"approval_timeout":  124,
		"command_timeout":   124,
		"cancelled":         130,
		"ssh_failed":        255,
		"policy_error":      125,
		"unknown":           1,
	}
	for code, want := range tests {
		if got := exitCodeForAgentSSHError(code); got != want {
			t.Fatalf("exitCodeForAgentSSHError(%q)=%d want=%d", code, got, want)
		}
	}
}

func TestRunExecStreamsAndReturnsRemoteExit(t *testing.T) {
	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	connectFn = func() (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			dec := protocol.NewDecoder(serverConn)
			enc := protocol.NewEncoder(serverConn)
			var req protocol.Message
			if err := dec.Decode(&req); err != nil {
				return
			}
			_ = enc.Encode(protocol.Message{Type: "command.created", ID: "cmd"})
			_ = enc.Encode(protocol.Message{Type: "command.queued", ID: "cmd", Position: 2})
			_ = enc.Encode(protocol.Message{Type: "approval.waiting", ID: "cmd", PolicyDecision: &model.PolicyDecision{Risk: model.RiskHigh}})
			_ = enc.Encode(protocol.Message{Type: "approval.heartbeat", ID: "cmd", Elapsed: "30s", TUIConnected: true})
			_ = enc.Encode(protocol.Chunk("stdout_chunk", "cmd", 1, []byte("out")))
			_ = enc.Encode(protocol.Chunk("stderr_chunk", "cmd", 1, []byte("err")))
			code := 3
			_ = enc.Encode(protocol.Message{Type: "final", OK: true, ID: "cmd", RemoteExitCode: &code})
			_ = req
		}()
		return clientConn, nil
	}
	code := RunExec([]string{"prod", "--", "uptime"})
	if code != 3 {
		t.Fatalf("code=%d", code)
	}
	if out.String() != "out" || !strings.Contains(errOut.String(), "err") || !strings.Contains(errOut.String(), "queued") || !strings.Contains(errOut.String(), "waiting") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestRunExecAgentSSHErrors(t *testing.T) {
	tests := []struct {
		name string
		msg  protocol.Message
		want int
	}{
		{"final", protocol.Message{Type: "final", OK: false, Error: "blocked", AgentSSHErrorCode: "policy_blocked"}, 126},
		{"error", protocol.Message{Type: "error", Error: "timeout", AgentSSHErrorCode: "approval_timeout"}, 124},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			restore := stubCLI(t, &out, &errOut)
			defer restore()
			connectFn = func() (net.Conn, error) {
				clientConn, serverConn := net.Pipe()
				go func() {
					defer serverConn.Close()
					dec := protocol.NewDecoder(serverConn)
					enc := protocol.NewEncoder(serverConn)
					var req protocol.Message
					_ = dec.Decode(&req)
					_ = enc.Encode(tt.msg)
				}()
				return clientConn, nil
			}
			if code := RunExec([]string{"prod", "--", "uptime"}); code != tt.want {
				t.Fatalf("code=%d want=%d stderr=%q", code, tt.want, errOut.String())
			}
		})
	}
}

func TestRunExecSetupErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	ensureDaemonFn = func(context.Context) error { return errors.New("daemon bad") }
	if code := RunExec([]string{"prod", "--", "uptime"}); code != 1 {
		t.Fatalf("ensure code=%d", code)
	}
	ensureDaemonFn = func(context.Context) error { return nil }
	connectFn = func() (net.Conn, error) { return nil, errors.New("connect bad") }
	if code := RunExec([]string{"prod", "--", "uptime"}); code != 1 {
		t.Fatalf("connect code=%d", code)
	}
}

func TestRunExecDecodeErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	connectFn = func() (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			dec := protocol.NewDecoder(serverConn)
			var req protocol.Message
			_ = dec.Decode(&req)
			_, _ = serverConn.Write([]byte(`{"type":"stdout_chunk","encoding":"base64","data":"!!!"}` + "\n"))
		}()
		return clientConn, nil
	}
	if code := RunExec([]string{"prod", "--", "uptime"}); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}

func stubCLI(t *testing.T, out, errOut *bytes.Buffer) func() {
	t.Helper()
	origEnsure := ensureDaemonFn
	origConnect := connectFn
	origStdout := stdout
	origStderr := stderr
	origGetwd := getwdFn
	origGetenv := getenvFn
	origGetpid := getpidFn
	origCancel := cancelRequestFn
	ensureDaemonFn = func(context.Context) error { return nil }
	connectFn = func() (net.Conn, error) { return nil, errors.New("connect not stubbed") }
	stdout = out
	stderr = errOut
	getwdFn = func() (string, error) { return "/work", nil }
	getenvFn = func(string) string { return "test" }
	getpidFn = func() int { return 42 }
	cancelRequestFn = func(string) {}
	return func() {
		ensureDaemonFn = origEnsure
		connectFn = origConnect
		stdout = origStdout
		stderr = origStderr
		getwdFn = origGetwd
		getenvFn = origGetenv
		getpidFn = origGetpid
		cancelRequestFn = origCancel
	}
}
