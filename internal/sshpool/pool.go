package sshpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
)

var (
	ErrTimeout   = errors.New("command timeout")
	ErrCancelled = errors.New("command cancelled")
)

type Pool struct {
	mu    sync.Mutex
	conns map[string]*pooledConn
}

type pooledConn struct {
	client       *ssh.Client
	hostName     string
	host         config.Host
	connectedAt  time.Time
	lastUsed     time.Time
	lastCommand  string
	openSessions int
	mu           sync.Mutex
}

type ExecOptions struct {
	CommandID    string
	HostName     string
	ResolvedHost config.Host
	Command      string
	Stdout       func([]byte)
	Stderr       func([]byte)
}

type ExecResult struct {
	RemoteExitCode *int
	Connection     model.ConnectionStatus
}

type UploadOptions struct {
	CommandID      string
	HostName       string
	ResolvedHost   config.Host
	RemotePath     string
	DisplayCommand string
	Reader         io.Reader
	Atomic         bool
	FileMode       os.FileMode
	PreserveMode   bool
	Validate       func(TransferResult) error
}

type DownloadOptions struct {
	HostName       string
	ResolvedHost   config.Host
	RemotePath     string
	DisplayCommand string
	Writer         io.Writer
	Ready          func(RemoteFileInfo) error
}

type RemoteFileInfo struct {
	Size int64
	Mode os.FileMode
}

type TransferResult struct {
	Bytes      int64
	Checksum   string
	Size       int64
	FileMode   os.FileMode
	Connection model.ConnectionStatus
}

func New() *Pool {
	return &Pool{conns: map[string]*pooledConn{}}
}

func (p *Pool) Execute(ctx context.Context, opts ExecOptions) (ExecResult, error) {
	pc, err := p.get(ctx, opts.HostName, opts.ResolvedHost)
	if err != nil {
		return ExecResult{}, err
	}

	pc.mu.Lock()
	pc.openSessions++
	pc.lastUsed = time.Now()
	pc.lastCommand = opts.Command
	pc.mu.Unlock()
	defer func() {
		pc.mu.Lock()
		pc.openSessions--
		pc.lastUsed = time.Now()
		pc.mu.Unlock()
	}()

	session, err := pc.client.NewSession()
	if err != nil {
		p.drop(opts.HostName)
		return ExecResult{}, err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return ExecResult{}, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return ExecResult{}, err
	}

	if err := session.Start(opts.Command); err != nil {
		return ExecResult{}, err
	}

	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go copyChunks(&copyWG, stdout, opts.Stdout)
	go copyChunks(&copyWG, stderr, opts.Stderr)

	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		_ = session.Close()
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ExecResult{Connection: p.connectionStatus(pc)}, ErrTimeout
			}
			return ExecResult{Connection: p.connectionStatus(pc)}, ErrCancelled
		}
		copyWG.Wait()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ExecResult{Connection: p.connectionStatus(pc)}, ErrTimeout
		}
		return ExecResult{Connection: p.connectionStatus(pc)}, ErrCancelled
	}
	copyWG.Wait()

	result := ExecResult{Connection: p.connectionStatus(pc)}
	if waitErr == nil {
		code := 0
		result.RemoteExitCode = &code
		return result, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitStatus()
		result.RemoteExitCode = &code
		return result, waitErr
	}
	p.drop(opts.HostName)
	return result, waitErr
}

func (p *Pool) Upload(ctx context.Context, opts UploadOptions) (TransferResult, error) {
	pc, client, release, err := p.openSFTP(ctx, opts.HostName, opts.ResolvedHost, opts.DisplayCommand)
	if err != nil {
		return TransferResult{}, err
	}
	defer release()

	existed := false
	if info, statErr := client.Lstat(opts.RemotePath); statErr == nil {
		if info.IsDir() {
			return TransferResult{}, fmt.Errorf("remote destination %s is a directory", opts.RemotePath)
		}
		existed = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return TransferResult{}, fmt.Errorf("stat remote destination %s: %w", opts.RemotePath, statErr)
	}

	writePath := opts.RemotePath
	if opts.Atomic {
		if _, ok := client.HasExtension("posix-rename@openssh.com"); !ok {
			return TransferResult{}, fmt.Errorf("remote SFTP server does not support atomic replacement")
		}
		writePath = atomicTempPath(opts.RemotePath, opts.CommandID)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if opts.Atomic {
		flags |= os.O_EXCL
	}
	file, err := client.OpenFile(writePath, flags)
	if err != nil {
		return TransferResult{}, fmt.Errorf("open remote destination %s: %w", writePath, err)
	}
	stopCancellation := closeSFTPOnCancel(ctx, client)
	defer stopCancellation()

	complete := false
	defer func() {
		_ = file.Close()
		if !complete && (opts.Atomic || !existed) {
			if err := client.Remove(writePath); err != nil {
				if cleanup, openErr := sftp.NewClient(pc.client); openErr == nil {
					_ = cleanup.Remove(writePath)
					_ = cleanup.Close()
				}
			}
		}
	}()

	hash := sha256.New()
	written, copyErr := copyWithContext(ctx, io.MultiWriter(file, hash), opts.Reader)
	if copyErr != nil {
		return TransferResult{}, transferError(ctx, fmt.Errorf("write remote destination %s: %w", writePath, copyErr))
	}
	if err := file.Close(); err != nil {
		return TransferResult{}, fmt.Errorf("close remote destination %s: %w", writePath, err)
	}
	if opts.PreserveMode {
		if err := client.Chmod(writePath, opts.FileMode.Perm()); err != nil {
			return TransferResult{}, fmt.Errorf("chmod remote destination %s: %w", writePath, err)
		}
	}
	result := TransferResult{
		Bytes:      written,
		Checksum:   hex.EncodeToString(hash.Sum(nil)),
		Size:       written,
		FileMode:   opts.FileMode.Perm(),
		Connection: p.connectionStatus(pc),
	}
	if opts.Validate != nil {
		if err := opts.Validate(result); err != nil {
			return result, err
		}
	}
	if opts.Atomic {
		if err := client.PosixRename(writePath, opts.RemotePath); err != nil {
			return TransferResult{}, fmt.Errorf("atomically replace remote destination %s: %w", opts.RemotePath, err)
		}
	}
	complete = true

	return result, nil
}

func (p *Pool) Download(ctx context.Context, opts DownloadOptions) (TransferResult, error) {
	pc, client, release, err := p.openSFTP(ctx, opts.HostName, opts.ResolvedHost, opts.DisplayCommand)
	if err != nil {
		return TransferResult{}, err
	}
	defer release()

	info, err := client.Stat(opts.RemotePath)
	if err != nil {
		return TransferResult{}, fmt.Errorf("stat remote source %s: %w", opts.RemotePath, err)
	}
	if !info.Mode().IsRegular() {
		return TransferResult{}, fmt.Errorf("remote source %s is not a regular file", opts.RemotePath)
	}
	remoteInfo := RemoteFileInfo{Size: info.Size(), Mode: info.Mode().Perm()}
	if opts.Ready != nil {
		if err := opts.Ready(remoteInfo); err != nil {
			return TransferResult{}, err
		}
	}

	file, err := client.Open(opts.RemotePath)
	if err != nil {
		return TransferResult{}, fmt.Errorf("open remote source %s: %w", opts.RemotePath, err)
	}
	defer file.Close()
	stopCancellation := closeSFTPOnCancel(ctx, client)
	defer stopCancellation()

	hash := sha256.New()
	written, copyErr := copyWithContext(ctx, io.MultiWriter(opts.Writer, hash), file)
	if copyErr != nil {
		return TransferResult{}, transferError(ctx, fmt.Errorf("read remote source %s: %w", opts.RemotePath, copyErr))
	}
	return TransferResult{
		Bytes:      written,
		Checksum:   hex.EncodeToString(hash.Sum(nil)),
		Size:       info.Size(),
		FileMode:   info.Mode().Perm(),
		Connection: p.connectionStatus(pc),
	}, nil
}

func (p *Pool) openSFTP(ctx context.Context, hostName string, host config.Host, displayCommand string) (*pooledConn, *sftp.Client, func(), error) {
	pc, err := p.get(ctx, hostName, host)
	if err != nil {
		return nil, nil, nil, err
	}

	pc.mu.Lock()
	pc.openSessions++
	pc.lastUsed = time.Now()
	pc.lastCommand = displayCommand
	pc.mu.Unlock()

	client, err := sftp.NewClient(pc.client)
	if err != nil {
		pc.mu.Lock()
		pc.openSessions--
		pc.lastUsed = time.Now()
		pc.mu.Unlock()
		p.drop(hostName)
		return nil, nil, nil, err
	}
	release := func() {
		_ = client.Close()
		pc.mu.Lock()
		pc.openSessions--
		pc.lastUsed = time.Now()
		pc.mu.Unlock()
	}
	return pc, client, release, nil
}

func atomicTempPath(destination, seed string) string {
	name := path.Base(destination)
	if seed == "" {
		seed = fmt.Sprint(time.Now().UnixNano())
	}
	seed = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, seed)
	if len(seed) > 32 {
		seed = seed[len(seed)-32:]
	}
	return path.Join(path.Dir(destination), "."+name+".ssh-use-"+seed+".tmp")
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	return io.CopyBuffer(contextWriter{ctx: ctx, writer: dst}, contextReader{ctx: ctx, reader: src}, buf)
}

func closeSFTPOnCancel(ctx context.Context, client *sftp.Client) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()
	return func() {
		once.Do(func() { close(done) })
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(data)
	}
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	default:
		return w.writer.Write(data)
	}
}

func transferError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ErrCancelled
	}
	return err
}

func (p *Pool) Statuses() []model.ConnectionStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	statuses := make([]model.ConnectionStatus, 0, len(p.conns))
	for _, pc := range p.conns {
		statuses = append(statuses, p.connectionStatus(pc))
	}
	return statuses
}

func (p *Pool) CloseIdle(maxIdle time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, pc := range p.conns {
		pc.mu.Lock()
		idle := now.Sub(pc.lastUsed)
		open := pc.openSessions
		pc.mu.Unlock()
		if open == 0 && idle > maxIdle {
			_ = pc.client.Close()
			delete(p.conns, key)
		}
	}
}

func (p *Pool) CloseAllIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, pc := range p.conns {
		pc.mu.Lock()
		open := pc.openSessions
		pc.mu.Unlock()
		if open == 0 {
			_ = pc.client.Close()
			delete(p.conns, key)
		}
	}
}

func (p *Pool) CloseHostIdle(hostName string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	closed := false
	for key, pc := range p.conns {
		if pc.hostName != hostName {
			continue
		}
		pc.mu.Lock()
		open := pc.openSessions
		pc.mu.Unlock()
		if open == 0 {
			_ = pc.client.Close()
			delete(p.conns, key)
			closed = true
		}
	}
	return closed
}

func (p *Pool) ApplyConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	var clients []*ssh.Client
	p.mu.Lock()
	for key, pc := range p.conns {
		host, err := cfg.ResolveHost(pc.hostName)
		if err == nil {
			var desired string
			desired, err = connKey(pc.hostName, host)
			if err == nil && desired == key {
				continue
			}
		}
		pc.mu.Lock()
		open := pc.openSessions
		pc.mu.Unlock()
		if open == 0 {
			clients = append(clients, pc.client)
			delete(p.conns, key)
		}
	}
	p.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

func (p *Pool) get(ctx context.Context, hostName string, host config.Host) (*pooledConn, error) {
	key, keyData, err := connectionIdentity(hostName, host)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if pc := p.conns[key]; pc != nil {
		p.mu.Unlock()
		return pc, nil
	}
	p.mu.Unlock()

	client, err := dial(ctx, host, keyData)
	if err != nil {
		return nil, err
	}
	pc := &pooledConn{client: client, hostName: hostName, host: host, connectedAt: time.Now(), lastUsed: time.Now()}

	p.mu.Lock()
	if existing := p.conns[key]; existing != nil {
		p.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	p.conns[key] = pc
	p.mu.Unlock()
	return pc, nil
}

func (p *Pool) drop(hostName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, pc := range p.conns {
		if pc.hostName == hostName {
			_ = pc.client.Close()
			delete(p.conns, key)
		}
	}
}

func dial(ctx context.Context, host config.Host, keyData []byte) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w; encrypted keys are not supported in MVP", host.Key, err)
	}
	hostKeyCallback, err := knownHostCallback()
	if err != nil {
		return nil, err
	}
	sshConfig := &ssh.ClientConfig{
		User:            host.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         host.ConnectTimeout,
	}
	addr := fmt.Sprintf("%s:%d", host.Addr, host.Port)
	dialer := net.Dialer{Timeout: host.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if host.ConnectTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(host.ConnectTimeout))
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func knownHostCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("cannot resolve home directory for known_hosts")
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", path, err)
	}
	return callback, nil
}

func connKey(name string, host config.Host) (string, error) {
	key, _, err := connectionIdentity(name, host)
	return key, err
}

func connectionIdentity(name string, host config.Host) (string, []byte, error) {
	data, err := os.ReadFile(host.Key)
	if err != nil {
		return "", nil, fmt.Errorf("read ssh key %s: %w", host.Key, err)
	}
	digest := sha256.Sum256(data)
	keyIdentity := host.Key + "#" + hex.EncodeToString(digest[:])
	return fmt.Sprintf("%s|%s|%s|%d|%s", name, host.User, host.Addr, host.Port, keyIdentity), data, nil
}

func copyChunks(wg *sync.WaitGroup, r io.Reader, emit func([]byte)) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 && emit != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			emit(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (p *Pool) connectionStatus(pc *pooledConn) model.ConnectionStatus {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return model.ConnectionStatus{
		Host:         pc.hostName,
		Status:       "CONNECTED",
		User:         pc.host.User,
		Addr:         fmt.Sprintf("%s:%d", pc.host.Addr, pc.host.Port),
		LastUsed:     pc.lastUsed,
		ConnectedAt:  pc.connectedAt,
		OpenSessions: pc.openSessions,
		LastCommand:  pc.lastCommand,
	}
}
