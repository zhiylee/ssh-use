package sshpool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zhiylee/ssh-use/internal/config"
)

func TestConnKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := config.Host{Addr: "1.2.3.4", User: "root", Port: 22, Key: path}
	got, err := connKey("prod", host)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "prod|root|1.2.3.4|22|"+path+"#") {
		t.Fatalf("connKey=%q", got)
	}
}

func TestConnKeyChangesWhenPrivateKeyRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := config.Host{Addr: "1.2.3.4", User: "root", Port: 22, Key: path}
	first, err := connKey("prod", host)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := connKey("prod", host)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("private key rotation did not change connection identity")
	}
}

func TestNewPoolAndEmptyStatuses(t *testing.T) {
	p := New()
	if len(p.Statuses()) != 0 {
		t.Fatal("expected no statuses")
	}
	p.CloseIdle(time.Nanosecond)
	p.CloseAllIdle()
	if p.CloseHostIdle("missing") {
		t.Fatal("closed missing host")
	}
}

func TestCopyChunks(t *testing.T) {
	var wg sync.WaitGroup
	var got strings.Builder
	wg.Add(1)
	copyChunks(&wg, strings.NewReader("hello"), func(data []byte) {
		got.Write(data)
	})
	w := make(chan struct{})
	go func() { wg.Wait(); close(w) }()
	select {
	case <-w:
	case <-time.After(time.Second):
		t.Fatal("copy did not finish")
	}
	if got.String() != "hello" {
		t.Fatalf("got %q", got.String())
	}

	wg.Add(1)
	copyChunks(&wg, errReader{}, nil)
	wg.Wait()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestExecuteWithInProcessSSHServer(t *testing.T) {
	fixture := newSSHFixture(t)
	p := New()

	var stdout, stderr bytes.Buffer
	result, err := p.Execute(context.Background(), ExecOptions{
		HostName:     "test",
		ResolvedHost: fixture.host,
		Command:      "ok",
		Stdout:       func(data []byte) { stdout.Write(data) },
		Stderr:       func(data []byte) { stderr.Write(data) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteExitCode == nil || *result.RemoteExitCode != 0 {
		t.Fatalf("exit=%v", result.RemoteExitCode)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(p.Statuses()) != 1 || p.Statuses()[0].OpenSessions != 0 {
		t.Fatalf("statuses=%#v", p.Statuses())
	}

	result, err = p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "fail"})
	if err == nil {
		t.Fatal("expected remote exit error")
	}
	if result.RemoteExitCode == nil || *result.RemoteExitCode != 7 {
		t.Fatalf("exit=%v err=%v", result.RemoteExitCode, err)
	}
	if !p.CloseHostIdle("test") {
		t.Fatal("expected idle connection to close")
	}
	if len(p.Statuses()) != 0 {
		t.Fatalf("statuses after close=%#v", p.Statuses())
	}
}

func TestApplyConfigClosesOnlyChangedIdleConnection(t *testing.T) {
	fixture := newSSHFixture(t)
	p := New()
	if _, err := p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "ok"}); err != nil {
		t.Fatal(err)
	}
	unchanged := config.Default()
	unchanged.Hosts["test"] = fixture.host
	p.ApplyConfig(unchanged)
	if statuses := p.Statuses(); len(statuses) != 1 {
		t.Fatalf("unchanged connection was closed: %#v", statuses)
	}

	changed := fixture.host
	changed.User = "other-user"
	cfg := config.Default()
	cfg.Hosts["test"] = changed
	p.ApplyConfig(cfg)
	if statuses := p.Statuses(); len(statuses) != 0 {
		t.Fatalf("stale idle connection was retained: %#v", statuses)
	}
	if _, err := p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: changed, Command: "ok"}); err != nil {
		t.Fatal(err)
	}

	statuses := p.Statuses()
	if len(statuses) != 1 || statuses[0].User != changed.User {
		t.Fatalf("changed host connection = %#v", statuses)
	}
}

func TestExecuteTimeout(t *testing.T) {
	fixture := newSSHFixture(t)
	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Execute(ctx, ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "hang"})
	if err != ErrTimeout {
		t.Fatalf("err=%v", err)
	}
}

func TestSFTPUploadDownloadAndAtomicValidation(t *testing.T) {
	fixture := newSSHFixture(t)
	p := New()
	remoteDir := t.TempDir()
	remotePath := filepath.Join(remoteDir, "file.bin")
	data := append([]byte{0, 1, 2, 255}, bytes.Repeat([]byte("payload"), 30000)...)

	upload, err := p.Upload(context.Background(), UploadOptions{
		CommandID:      "direct",
		HostName:       "test",
		ResolvedHost:   fixture.host,
		RemotePath:     remotePath,
		DisplayCommand: "cp local test:/file.bin",
		Reader:         bytes.NewReader(data),
		FileMode:       0o640,
		PreserveMode:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if upload.Bytes != int64(len(data)) || upload.Checksum == "" {
		t.Fatalf("upload=%#v", upload)
	}
	remoteData, err := os.ReadFile(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remoteData, data) {
		t.Fatalf("remote data mismatch: got=%d want=%d", len(remoteData), len(data))
	}
	if info, err := os.Stat(remotePath); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("remote mode info=%v err=%v", info, err)
	}

	var downloaded bytes.Buffer
	var ready RemoteFileInfo
	download, err := p.Download(context.Background(), DownloadOptions{
		HostName:       "test",
		ResolvedHost:   fixture.host,
		RemotePath:     remotePath,
		DisplayCommand: "cp test:/file.bin local",
		Writer:         &downloaded,
		Ready: func(info RemoteFileInfo) error {
			ready = info
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded.Bytes(), data) || download.Checksum != upload.Checksum || ready.Size != int64(len(data)) || ready.Mode != 0o640 {
		t.Fatalf("download=%#v ready=%#v bytes=%d", download, ready, downloaded.Len())
	}

	old := []byte("old contents")
	if err := os.WriteFile(remotePath, old, 0o600); err != nil {
		t.Fatal(err)
	}
	validationErr := errors.New("reject checksum")
	_, err = p.Upload(context.Background(), UploadOptions{
		CommandID:      "atomic-rejected",
		HostName:       "test",
		ResolvedHost:   fixture.host,
		RemotePath:     remotePath,
		DisplayCommand: "cp --atomic local test:/file.bin",
		Reader:         bytes.NewReader(data),
		Atomic:         true,
		Validate: func(TransferResult) error {
			return validationErr
		},
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("atomic validation err=%v", err)
	}
	remoteData, err = os.ReadFile(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remoteData, old) {
		t.Fatalf("atomic validation replaced destination: %q", remoteData)
	}

	_, err = p.Upload(context.Background(), UploadOptions{
		CommandID:      "atomic-ok",
		HostName:       "test",
		ResolvedHost:   fixture.host,
		RemotePath:     remotePath,
		DisplayCommand: "cp --atomic local test:/file.bin",
		Reader:         bytes.NewReader(data),
		Atomic:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	remoteData, err = os.ReadFile(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remoteData, data) {
		t.Fatal("atomic upload did not replace destination")
	}
}

func TestIdleCloseAndDrop(t *testing.T) {
	fixture := newSSHFixture(t)
	p := New()
	if _, err := p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "ok"}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for _, pc := range p.conns {
		pc.mu.Lock()
		pc.lastUsed = time.Now().Add(-time.Hour)
		pc.mu.Unlock()
	}
	p.mu.Unlock()
	p.CloseIdle(time.Second)
	if len(p.Statuses()) != 0 {
		t.Fatalf("statuses=%#v", p.Statuses())
	}

	if _, err := p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "ok"}); err != nil {
		t.Fatal(err)
	}
	p.drop("test")
	if len(p.Statuses()) != 0 {
		t.Fatalf("statuses after drop=%#v", p.Statuses())
	}

	if _, err := p.Execute(context.Background(), ExecOptions{HostName: "test", ResolvedHost: fixture.host, Command: "ok"}); err != nil {
		t.Fatal(err)
	}
	p.CloseAllIdle()
	if len(p.Statuses()) != 0 {
		t.Fatalf("statuses after close all=%#v", p.Statuses())
	}
}

type sshFixture struct {
	host config.Host
}

func newSSHFixture(t *testing.T) sshFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKeyPath := filepath.Join(dir, "client_key")
	clientPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	if err := os.WriteFile(clientKeyPath, clientPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverSigner, err := ssh.NewSignerFromKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	knownLine := knownhosts.Line([]string{listener.Addr().String()}, serverSigner.PublicKey())
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(knownLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unauthorized")
	}}
	serverConfig.AddHostKey(serverSigner)
	go serveTestSSH(listener, serverConfig)

	addr, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	for _, r := range portText {
		port = port*10 + int(r-'0')
	}
	cfg := config.Default()
	cfg.Hosts["test"] = config.Host{Addr: addr, User: "tester", Port: port, Key: clientKeyPath}
	cfg.Defaults.ConnectTimeout.Duration = time.Second
	cfg.Defaults.CommandTimeout.Duration = time.Second
	host, err := cfg.ResolveHost("test")
	if err != nil {
		t.Fatal(err)
	}
	return sshFixture{host: host}
}

func serveTestSSH(listener net.Listener, cfg *ssh.ServerConfig) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleTestSSHConn(conn, cfg)
	}
}

func handleTestSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go handleTestSession(channel, requests)
	}
}

func handleTestSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			ssh.Unmarshal(req.Payload, &payload)
			req.Reply(true, nil)
			switch payload.Command {
			case "ok":
				channel.Write([]byte("out\n"))
				channel.Stderr().Write([]byte("err\n"))
				sendExitStatus(channel, 0)
				return
			case "fail":
				sendExitStatus(channel, 7)
				return
			case "hang":
				time.Sleep(5 * time.Second)
				return
			default:
				sendExitStatus(channel, 0)
				return
			}
		case "subsystem":
			var payload struct{ Name string }
			ssh.Unmarshal(req.Payload, &payload)
			if payload.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			server, err := sftp.NewServer(channel)
			if err != nil {
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)
			_ = server.Serve()
			_ = server.Close()
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func sendExitStatus(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: status}))
}
