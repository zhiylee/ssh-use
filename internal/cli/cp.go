package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/zhiylee/ssh-use/internal/protocol"
)

var cpStdin io.Reader = os.Stdin

type copySpec struct {
	atomic      bool
	direction   string
	host        string
	localPath   string
	remotePath  string
	source      string
	destination string
}

type copyEndpoint struct {
	remote bool
	host   string
	path   string
}

func RunCopy(args []string) int {
	spec, err := parseCopyArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: %v\n", err)
		return 2
	}

	var source io.Reader
	var sourceFile *os.File
	size := int64(-1)
	var fileMode uint32
	preserveMode := false
	if spec.direction == "upload" {
		if spec.localPath == "-" {
			source = cpStdin
		} else {
			sourceFile, err = os.Open(spec.localPath)
			if err != nil {
				fmt.Fprintf(stderr, "ssh-use: open local source %s: %v\n", spec.localPath, err)
				return 1
			}
			defer sourceFile.Close()
			info, statErr := sourceFile.Stat()
			if statErr != nil {
				fmt.Fprintf(stderr, "ssh-use: stat local source %s: %v\n", spec.localPath, statErr)
				return 1
			}
			if !info.Mode().IsRegular() {
				fmt.Fprintf(stderr, "ssh-use: local source %s is not a regular file\n", spec.localPath)
				return 1
			}
			source = sourceFile
			size = info.Size()
			fileMode = uint32(info.Mode().Perm())
			preserveMode = true
		}
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
	var interrupted atomic.Bool
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	defer close(done)
	go func() {
		select {
		case <-sigCh:
			interrupted.Store(true)
			id, _ := commandID.Load().(string)
			if id != "" {
				cancelRequestFn(id)
			}
			_ = conn.Close()
		case <-done:
		}
	}()

	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	cwd, _ := getwdFn()
	sourceName := getenvFn("SSH_USE_SOURCE")
	if sourceName == "" {
		sourceName = "unknown"
	}
	if err := enc.Encode(protocol.Message{
		Type:         "file.transfer",
		Host:         spec.host,
		Source:       sourceName,
		CWD:          cwd,
		ClientPID:    getpidFn(),
		Direction:    spec.direction,
		LocalPath:    spec.localPath,
		RemotePath:   spec.remotePath,
		Size:         size,
		FileMode:     fileMode,
		PreserveMode: preserveMode,
		Atomic:       spec.atomic,
	}); err != nil {
		fmt.Fprintf(stderr, "ssh-use: send transfer request: %v\n", err)
		return 1
	}

	var sink *downloadSink
	uploadSent := false
	var downloadSeq int64
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			if sink != nil {
				sink.abort()
			}
			if interrupted.Load() {
				return 130
			}
			fmt.Fprintf(stderr, "ssh-use: daemon connection closed: %v\n", err)
			return 1
		}
		if msg.ID != "" {
			commandID.Store(msg.ID)
		}

		switch msg.Type {
		case "command.created":
			commandID.Store(msg.ID)
		case "command.queued":
			if msg.Position > 0 {
				fmt.Fprintf(stderr, "ssh-use: queued on %s position=%d\n", spec.host, msg.Position)
			} else {
				fmt.Fprintf(stderr, "ssh-use: queued on %s\n", spec.host)
			}
		case "approval.waiting":
			fmt.Fprintln(stderr, "ssh-use: waiting for user approval")
			fmt.Fprintln(stderr, "open console: ssh-use tui")
			risk := ""
			if msg.PolicyDecision != nil {
				risk = string(msg.PolicyDecision.Risk)
			}
			fmt.Fprintf(stderr, "id: %s\nhost: %s\nrisk: %s\ncopy: %s -> %s\n", msg.ID, spec.host, risk, spec.source, spec.destination)
		case "approval.heartbeat":
			if msg.TUIConnected {
				fmt.Fprintf(stderr, "ssh-use: waiting for approval in TUI id=%s elapsed=%s\n", msg.ID, msg.Elapsed)
			} else {
				fmt.Fprintf(stderr, "ssh-use: still waiting for approval id=%s elapsed=%s open_console=\"ssh-use tui\"\n", msg.ID, msg.Elapsed)
			}
		case "transfer.ready":
			if spec.direction == "upload" {
				if uploadSent {
					fmt.Fprintln(stderr, "ssh-use: duplicate transfer.ready")
					return 1
				}
				uploadSent = true
				if err := streamUpload(enc, msg.ID, source); err != nil {
					fmt.Fprintf(stderr, "ssh-use: upload source: %v\n", err)
				}
				continue
			}
			if sink != nil {
				fmt.Fprintln(stderr, "ssh-use: duplicate transfer.ready")
				return 1
			}
			sink, err = newDownloadSink(spec.localPath, spec.remotePath, spec.atomic, os.FileMode(msg.FileMode))
			if err != nil {
				sink = failedDownloadSink(err)
			}
		case "transfer.chunk":
			if sink == nil {
				fmt.Fprintln(stderr, "ssh-use: received file data before transfer.ready")
				return 1
			}
			expectedID, _ := commandID.Load().(string)
			if msg.ID != expectedID {
				sink.fail(fmt.Errorf("transfer chunk has unexpected id %q", msg.ID))
				continue
			}
			if msg.Seq != downloadSeq+1 {
				sink.fail(fmt.Errorf("transfer chunk sequence %d, expected %d", msg.Seq, downloadSeq+1))
				continue
			}
			downloadSeq = msg.Seq
			data, decodeErr := protocol.DecodeData(msg)
			if decodeErr != nil {
				sink.fail(fmt.Errorf("decode transfer chunk: %w", decodeErr))
				continue
			}
			sink.consume(data)
		case "transfer.eof":
			if sink == nil {
				fmt.Fprintln(stderr, "ssh-use: received transfer.eof before transfer.ready")
				return 1
			}
			expectedID, _ := commandID.Load().(string)
			if msg.ID != expectedID {
				sink.fail(fmt.Errorf("transfer.eof has unexpected id %q", msg.ID))
			}
			finishErr := sink.finish(msg.Bytes, msg.Checksum)
			commit := protocol.Message{Type: "transfer.commit", ID: msg.ID, OK: finishErr == nil}
			if finishErr != nil {
				commit.Error = finishErr.Error()
			}
			if err := enc.Encode(commit); err != nil {
				fmt.Fprintf(stderr, "ssh-use: confirm download: %v\n", err)
				return 1
			}
		case "final":
			localMayBeIncomplete := sink != nil && !sink.finalized && sink.existed && !spec.atomic
			if sink != nil && !sink.finalized {
				sink.abort()
			}
			if msg.OK {
				return 0
			}
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
				if spec.direction == "upload" && !spec.atomic {
					fmt.Fprintln(stderr, "ssh-use: remote destination may be incomplete")
				} else if spec.direction == "download" && localMayBeIncomplete {
					fmt.Fprintln(stderr, "ssh-use: local destination may be incomplete")
				}
			}
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		case "error":
			if sink != nil {
				sink.abort()
			}
			if msg.Error != "" {
				fmt.Fprintf(stderr, "ssh-use: %s\n", msg.Error)
			}
			return exitCodeForSSHUseError(msg.SSHUseErrorCode)
		}
	}
}

func parseCopyArgs(args []string) (copySpec, error) {
	var spec copySpec
	positional := make([]string, 0, 2)
	parseOptions := true
	for _, arg := range args {
		if parseOptions {
			switch arg {
			case "--atomic":
				spec.atomic = true
				continue
			case "--":
				parseOptions = false
				continue
			case "-":
				parseOptions = false
			default:
				if strings.HasPrefix(arg, "-") {
					return copySpec{}, fmt.Errorf("unknown cp option %q", arg)
				}
				parseOptions = false
			}
		}
		positional = append(positional, arg)
	}
	if len(positional) != 2 {
		return copySpec{}, fmt.Errorf("usage: ssh-use cp [--atomic] <source> <destination>")
	}

	source, err := parseCopyEndpoint(positional[0])
	if err != nil {
		return copySpec{}, err
	}
	destination, err := parseCopyEndpoint(positional[1])
	if err != nil {
		return copySpec{}, err
	}
	if source.remote == destination.remote {
		if source.remote {
			return copySpec{}, fmt.Errorf("remote-to-remote copies are not supported")
		}
		return copySpec{}, fmt.Errorf("exactly one endpoint must be remote")
	}
	if spec.atomic && !destination.remote && destination.path == "-" {
		return copySpec{}, fmt.Errorf("--atomic cannot be used when downloading to stdout")
	}

	spec.source = positional[0]
	spec.destination = positional[1]
	if destination.remote {
		spec.direction = "upload"
		spec.host = destination.host
		spec.localPath = source.path
		spec.remotePath = destination.path
	} else {
		spec.direction = "download"
		spec.host = source.host
		spec.localPath = destination.path
		spec.remotePath = source.path
	}
	return spec, nil
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	if value == "" {
		return copyEndpoint{}, fmt.Errorf("copy endpoint is empty")
	}
	if value == "-" {
		return copyEndpoint{path: value}, nil
	}
	separator := strings.Index(value, ":/")
	if separator <= 0 || strings.Contains(value[:separator], "/") {
		return copyEndpoint{path: value}, nil
	}
	host := value[:separator]
	if strings.Contains(host, "@") {
		return copyEndpoint{}, fmt.Errorf("remote users must be configured by host alias; user@host paths are not supported")
	}
	remotePath := value[separator+1:]
	if !path.IsAbs(remotePath) {
		return copyEndpoint{}, fmt.Errorf("remote path must be absolute: %s", value)
	}
	return copyEndpoint{remote: true, host: host, path: path.Clean(remotePath)}, nil
}

func streamUpload(enc *protocol.Encoder, id string, source io.Reader) error {
	hasher := sha256.New()
	buffer := make([]byte, 128*1024)
	var seq, total int64
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			seq++
			chunk := buffer[:n]
			_, _ = hasher.Write(chunk)
			total += int64(n)
			if err := enc.Encode(protocol.Chunk("transfer.chunk", id, seq, chunk)); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return enc.Encode(protocol.Message{Type: "transfer.eof", ID: id, OK: true, Bytes: total, Checksum: hex.EncodeToString(hasher.Sum(nil))})
		}
		_ = enc.Encode(protocol.Message{Type: "transfer.abort", ID: id, Error: readErr.Error()})
		return readErr
	}
}

type downloadSink struct {
	target    string
	writePath string
	file      *os.File
	writer    io.Writer
	hash      hash.Hash
	mode      os.FileMode
	atomic    bool
	existed   bool
	stdout    bool
	bytes     int64
	err       error
	finalized bool
}

func newDownloadSink(localPath, remotePath string, atomicWrite bool, mode os.FileMode) (*downloadSink, error) {
	sink := &downloadSink{atomic: atomicWrite, hash: sha256.New(), mode: mode.Perm()}
	if localPath == "-" {
		sink.stdout = true
		sink.writer = stdout
		return sink, nil
	}

	target := localPath
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, path.Base(remotePath))
	}
	sink.target = target
	if info, err := os.Lstat(target); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("local destination %s is a directory", target)
		}
		sink.existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat local destination %s: %w", target, err)
	}

	if atomicWrite {
		file, err := os.CreateTemp(filepath.Dir(target), ".ssh-use-*.tmp")
		if err != nil {
			return nil, fmt.Errorf("create temporary destination for %s: %w", target, err)
		}
		sink.file = file
		sink.writer = file
		sink.writePath = file.Name()
		return sink, nil
	}

	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open local destination %s: %w", target, err)
	}
	sink.file = file
	sink.writer = file
	sink.writePath = target
	return sink, nil
}

func failedDownloadSink(err error) *downloadSink {
	return &downloadSink{hash: sha256.New(), err: err}
}

func (s *downloadSink) consume(data []byte) {
	if s.err != nil {
		return
	}
	n, err := s.writer.Write(data)
	if n > 0 {
		_, _ = s.hash.Write(data[:n])
		s.bytes += int64(n)
	}
	if err != nil {
		s.err = err
		return
	}
	if n != len(data) {
		s.err = io.ErrShortWrite
	}
}

func (s *downloadSink) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

func (s *downloadSink) finish(expectedBytes int64, expectedChecksum string) error {
	if s.err == nil && s.bytes != expectedBytes {
		s.err = fmt.Errorf("download byte count mismatch: wrote %d, sender reported %d", s.bytes, expectedBytes)
	}
	if s.err == nil {
		actual := hex.EncodeToString(s.hash.Sum(nil))
		if expectedChecksum == "" || actual != expectedChecksum {
			s.err = fmt.Errorf("download checksum mismatch")
		}
	}
	if s.err == nil && s.file != nil {
		if err := s.file.Chmod(s.mode); err != nil {
			s.err = fmt.Errorf("chmod local destination %s: %w", s.writePath, err)
		}
	}
	if s.file != nil {
		if err := s.file.Close(); s.err == nil && err != nil {
			s.err = fmt.Errorf("close local destination %s: %w", s.writePath, err)
		}
		s.file = nil
	}
	if s.err == nil && s.atomic {
		if err := os.Rename(s.writePath, s.target); err != nil {
			s.err = fmt.Errorf("replace local destination %s: %w", s.target, err)
		}
	}
	if s.err != nil {
		s.abort()
		return s.err
	}
	s.finalized = true
	return nil
}

func (s *downloadSink) abort() {
	if s.finalized {
		return
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if s.atomic && s.writePath != "" {
		_ = os.Remove(s.writePath)
	} else if !s.stdout && !s.existed && s.target != "" {
		_ = os.Remove(s.target)
	}
}
