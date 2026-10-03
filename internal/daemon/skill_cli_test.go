package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/sshpool"
)

// Exercises the installed skill's CLI commands through the real authenticated
// gateway. The SSH executor is simulated; real SSH/SFTP has separate pool tests.
func TestAgentSkillCLIWorkflow(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ssh-use")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ssh-use")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	executor := &skillExecutor{fakeExecutor: fakeExecutor{stdout: []byte("file data\x00\xff\n")}}
	f := newRemoteFixture(t, func(server *Server) { server.pool = executor })
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "SSH_USE_") {
			env = append(env, item)
		}
	}
	env = append(env, "SSH_USE_CLIENT_CONFIG="+filepath.Join(f.dir, "agent", "client.yaml"), "SSH_USE_SERVER="+f.agent.Endpoint, "SSH_USE_CA_FILE="+f.agent.CAFile, "SSH_USE_TOKEN_FILE="+f.agent.TokenFile)
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		return cmd
	}
	run := func(args ...string) (string, string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := command(ctx, args...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("CLI %v: %v", args, err)
			}
			code = exit.ExitCode()
		}
		return out.String(), errOut.String(), code
	}
	if _, errOut, code := run("skill", "install", "--dir", filepath.Join(dir, "skills")); code != 0 {
		t.Fatalf("install: %d %s", code, errOut)
	}
	data, err := os.ReadFile(filepath.Join(dir, "skills", "ssh-use", "SKILL.md"))
	if err != nil || len(data) == 0 {
		t.Fatalf("installed skill: %v", err)
	}
	out, errOut, code := run("hosts", "list", "--json")
	var hosts protocol.Message
	if code != 0 || json.Unmarshal([]byte(out), &hosts) != nil || !hosts.OK || hosts.Hosts["h"].Addr == "" || executor.calls.Load() != 0 {
		t.Fatalf("host discovery: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run("exec", "--request-id", "skill-readonly", "h", "--", "uptime")
	if code != 0 || out != "remote output\n" {
		t.Fatalf("readonly: %d %q %s", code, out, errOut)
	}
	_, errOut, code = run("exec", "h", "--", "false")
	if code != 126 || !strings.Contains(errOut, "remote_exit_code=126") || strings.Contains(errOut, "ssh_use_error_code=") {
		t.Fatalf("remote exit origin: %d %s", code, errOut)
	}
	for _, decision := range []string{"approve", "reject"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := command(ctx, "exec", "--request-id", "skill-"+decision, "h", "--", "systemctl restart test-app")
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		before := executor.calls.Load()
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		id := ""
		for id == "" && ctx.Err() == nil {
			for _, record := range f.s.snapshot().Commands {
				if record.Status == model.StatusPendingApproval {
					id = record.ID
					break
				}
			}
			if id == "" {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if id == "" || executor.calls.Load() != before {
			cancel()
			<-done
			t.Fatal("sensitive command did not wait for human approval")
		}
		if resp := remoteRequest(t, f.agent, protocol.Message{Type: "approval.decide", ID: id, Decision: "approve"}); resp.OK {
			cancel()
			<-done
			t.Fatal("agent credential approved its own command")
		}
		if resp := remoteRequest(t, f.admin, protocol.Message{Type: "approval.decide", ID: id, Decision: decision}); !resp.OK {
			cancel()
			<-done
			t.Fatalf("fixture human decision: %#v", resp)
		}
		err := <-done
		cancel()
		if decision == "approve" {
			if err != nil || executor.calls.Load() != before+1 || out.String() != "remote output\n" {
				t.Fatalf("approved execution: %v %s", err, errOut.String())
			}
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 126 || executor.calls.Load() != before || !strings.Contains(errOut.String(), "ssh_use_error_code=approval_rejected") {
				t.Fatalf("rejected execution: %v %s", err, errOut.String())
			}
		}
		_, errOutString, lookupCode := run("jobs", "get", id)
		if lookupCode != 0 {
			t.Fatalf("agent job lookup: %d %s", lookupCode, errOutString)
		}
	}
	before := executor.calls.Load()
	if _, errOut, code := run("exec", "--request-id", "skill-approve", "h", "--", "systemctl restart test-app"); code != 0 || executor.calls.Load() != before {
		t.Fatalf("duplicate ran again: %d %s", code, errOut)
	}
	// File policy is independent of exec; auto mode belongs to the fixture
	// operator so these tests isolate the upload/download protocol itself.
	if err := f.s.setPolicyMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "upload with spaces.bin")
	if err := os.WriteFile(source, executor.stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := run("cp", "--atomic", "--", source, "h:/test/upload.bin"); code != 0 {
		t.Fatalf("upload: %d %s", code, errOut)
	}
	destination := filepath.Join(dir, "download with spaces.bin")
	if _, errOut, code := run("cp", "--atomic", "--", "h:/test/download.bin", destination); code != 0 {
		t.Fatalf("download: %d %s", code, errOut)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, executor.stdout) {
		t.Fatalf("download contents: %q %v", got, err)
	}
}

type skillExecutor struct {
	fakeExecutor
	calls atomic.Int64
}

func (f *skillExecutor) Execute(_ context.Context, opts sshpool.ExecOptions) (sshpool.ExecResult, error) {
	f.calls.Add(1)
	code := 0
	if opts.Command == "false" {
		code = 126 // Intentionally overlaps with approval_rejected.
	} else {
		opts.Stdout([]byte("remote output\n"))
	}
	return sshpool.ExecResult{RemoteExitCode: &code}, nil
}
