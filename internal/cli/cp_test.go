package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhiylee/ssh-use/internal/protocol"
)

func TestParseCopyArgs(t *testing.T) {
	upload, err := parseCopyArgs([]string{"./app.yaml", "prod:/etc/app.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if upload.direction != "upload" || upload.host != "prod" || upload.localPath != "./app.yaml" || upload.remotePath != "/etc/app.yaml" {
		t.Fatalf("upload=%#v", upload)
	}

	download, err := parseCopyArgs([]string{"--atomic", "prod:/var/log/app.log", "./app.log"})
	if err != nil {
		t.Fatal(err)
	}
	if download.direction != "download" || !download.atomic || download.host != "prod" || download.remotePath != "/var/log/app.log" {
		t.Fatalf("download=%#v", download)
	}

	stdinUpload, err := parseCopyArgs([]string{"-", "prod:/tmp/data"})
	if err != nil || stdinUpload.localPath != "-" || stdinUpload.direction != "upload" {
		t.Fatalf("stdin upload=%#v err=%v", stdinUpload, err)
	}
}

func TestParseCopyArgsErrors(t *testing.T) {
	tests := [][]string{
		{},
		{"one"},
		{"--bad", "a", "h:/b"},
		{"a", "b"},
		{"a:/x", "b:/y"},
		{"user@host:/x", "local"},
		{"--atomic", "host:/x", "-"},
	}
	for _, args := range tests {
		if _, err := parseCopyArgs(args); err == nil {
			t.Fatalf("expected error for %#v", args)
		}
	}
}

func TestParseCopyEndpointTreatsExplicitLocalPathAsLocal(t *testing.T) {
	endpoint, err := parseCopyEndpoint("./name:/part")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.remote || endpoint.path != "./name:/part" {
		t.Fatalf("endpoint=%#v", endpoint)
	}
}

func TestStreamUploadBinary(t *testing.T) {
	data := append([]byte{0, 1, 2, 255}, bytes.Repeat([]byte("x"), 200000)...)
	var wire bytes.Buffer
	if err := streamUpload(protocol.NewEncoder(&wire), "cmd", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	dec := protocol.NewDecoder(&wire)
	var received []byte
	var eof protocol.Message
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "transfer.eof" {
			eof = msg
			break
		}
		chunk, err := protocol.DecodeData(msg)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, chunk...)
	}
	if !bytes.Equal(received, data) || eof.Bytes != int64(len(data)) || eof.Checksum != checksum(data) {
		t.Fatalf("received=%d eof=%#v", len(received), eof)
	}
}

func TestDownloadSinkDirectAndAtomic(t *testing.T) {
	data := []byte("new contents\x00")
	dir := t.TempDir()

	directPath := filepath.Join(dir, "direct")
	direct, err := newDownloadSink(directPath, "/remote/direct", false, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	direct.consume(data)
	if err := direct.finish(int64(len(data)), checksum(data)); err != nil {
		t.Fatal(err)
	}
	assertLocalFile(t, directPath, data, 0o640)

	atomicPath := filepath.Join(dir, "atomic")
	if err := os.WriteFile(atomicPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	atomicSink, err := newDownloadSink(atomicPath, "/remote/atomic", true, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writePath := atomicSink.writePath
	atomicSink.consume(data)
	if err := atomicSink.finish(int64(len(data)), checksum(data)); err != nil {
		t.Fatal(err)
	}
	assertLocalFile(t, atomicPath, data, 0o644)
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists: %v", err)
	}
}

func TestDownloadSinkFailureCleanup(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new")
	sink, err := newDownloadSink(newPath, "/remote/new", false, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	sink.consume([]byte("partial"))
	if err := sink.finish(7, "wrong"); err == nil {
		t.Fatal("expected checksum failure")
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("new partial file was not removed: %v", err)
	}

	existingPath := filepath.Join(dir, "existing")
	if err := os.WriteFile(existingPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink, err = newDownloadSink(existingPath, "/remote/existing", false, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	sink.consume([]byte("partial"))
	if err := sink.finish(7, "wrong"); err == nil {
		t.Fatal("expected checksum failure")
	}
	got, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "partial" {
		t.Fatalf("existing partial file=%q", got)
	}
}

func TestRunCopyUpload(t *testing.T) {
	data := append([]byte{0, 1, 255}, bytes.Repeat([]byte("upload"), 30000)...)
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(sourcePath, data, 0o640); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	connectFn = func() (protocol.Connection, error) {
		clientConn, serverConn := net.Pipe()
		go serveUploadProtocol(t, serverConn, data)
		return clientConn, nil
	}

	if code := RunCopy([]string{"--atomic", sourcePath, "prod:/tmp/target.bin"}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}

func TestRunCopyDownload(t *testing.T) {
	data := append([]byte{0, 1, 255}, bytes.Repeat([]byte("download"), 30000)...)
	destination := filepath.Join(t.TempDir(), "target.bin")

	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	connectFn = func() (protocol.Connection, error) {
		clientConn, serverConn := net.Pipe()
		go serveDownloadProtocol(t, serverConn, data)
		return clientConn, nil
	}

	if code := RunCopy([]string{"prod:/tmp/source.bin", destination}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	assertLocalFile(t, destination, data, 0o640)
}

func TestRunCopyDownloadToStdout(t *testing.T) {
	data := []byte{0, 1, 2, 255, 'x'}
	var out, errOut bytes.Buffer
	restore := stubCLI(t, &out, &errOut)
	defer restore()
	connectFn = func() (protocol.Connection, error) {
		clientConn, serverConn := net.Pipe()
		go serveDownloadProtocol(t, serverConn, data)
		return clientConn, nil
	}

	if code := RunCopy([]string{"prod:/tmp/source.bin", "-"}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("stdout=%v want=%v", out.Bytes(), data)
	}
}

func TestRunCopyRefusalReportsOriginAndPreservesDestination(t *testing.T) {
	for _, messageType := range []string{"final", "error"} {
		t.Run(messageType, func(t *testing.T) {
			var out, errOut bytes.Buffer
			defer stubCLI(t, &out, &errOut)()
			destination := filepath.Join(t.TempDir(), "existing.bin")
			if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			connectFn = func() (protocol.Connection, error) {
				clientConn, serverConn := net.Pipe()
				go func() {
					defer serverConn.Close()
					var request protocol.Message
					if err := protocol.NewDecoder(serverConn).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					_ = protocol.NewEncoder(serverConn).Encode(protocol.Message{Type: messageType, ID: "copy-denied", Error: "refused", SSHUseErrorCode: "approval_rejected"})
				}()
				return clientConn, nil
			}
			if code := RunCopy([]string{"--atomic", "prod:/remote/file", destination}); code != 126 {
				t.Fatalf("copy: code=%d stderr=%s", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), "ssh_use_error_code=approval_rejected job_id=copy-denied") || out.Len() != 0 {
				t.Fatalf("failure origin: stdout=%s stderr=%s", out.String(), errOut.String())
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != "original" {
				t.Fatalf("refused copy modified destination: %q %v", data, err)
			}
		})
	}
}

func serveUploadProtocol(t *testing.T, conn net.Conn, want []byte) {
	t.Helper()
	defer conn.Close()
	dec := protocol.NewDecoder(conn)
	enc := protocol.NewEncoder(conn)
	var req protocol.Message
	if err := dec.Decode(&req); err != nil {
		t.Error(err)
		return
	}
	if req.Type != "file.transfer" || req.Direction != "upload" || !req.Atomic || req.Size != int64(len(want)) || !req.PreserveMode {
		t.Errorf("request=%#v", req)
		return
	}
	_ = enc.Encode(protocol.Message{Type: "command.created", ID: "cmd"})
	_ = enc.Encode(protocol.Message{Type: "transfer.ready", ID: "cmd"})
	var got []byte
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		if msg.Type == "transfer.eof" {
			if msg.Bytes != int64(len(want)) || msg.Checksum != checksum(want) || !bytes.Equal(got, want) {
				t.Errorf("eof=%#v bytes=%d", msg, len(got))
				return
			}
			break
		}
		chunk, err := protocol.DecodeData(msg)
		if err != nil {
			t.Error(err)
			return
		}
		got = append(got, chunk...)
	}
	_ = enc.Encode(protocol.Message{Type: "final", ID: "cmd", OK: true, Bytes: int64(len(want)), Checksum: checksum(want)})
}

func serveDownloadProtocol(t *testing.T, conn net.Conn, data []byte) {
	t.Helper()
	defer conn.Close()
	dec := protocol.NewDecoder(conn)
	enc := protocol.NewEncoder(conn)
	var req protocol.Message
	if err := dec.Decode(&req); err != nil {
		t.Error(err)
		return
	}
	if req.Type != "file.transfer" || req.Direction != "download" {
		t.Errorf("request=%#v", req)
		return
	}
	_ = enc.Encode(protocol.Message{Type: "command.created", ID: "cmd"})
	_ = enc.Encode(protocol.Message{Type: "transfer.ready", ID: "cmd", Size: int64(len(data)), FileMode: 0o640})
	_ = enc.Encode(protocol.Chunk("transfer.chunk", "cmd", 1, data))
	_ = enc.Encode(protocol.Message{Type: "transfer.eof", ID: "cmd", OK: true, Bytes: int64(len(data)), Checksum: checksum(data)})
	var commit protocol.Message
	if err := dec.Decode(&commit); err != nil {
		t.Error(err)
		return
	}
	if commit.Type != "transfer.commit" || !commit.OK {
		t.Errorf("commit=%#v", commit)
		return
	}
	_ = enc.Encode(protocol.Message{Type: "final", ID: "cmd", OK: true, Bytes: int64(len(data)), Checksum: checksum(data)})
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertLocalFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file data mismatch: got=%d want=%d", len(got), len(want))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("mode=%o want=%o", info.Mode().Perm(), mode)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestStreamUploadSendsAbort(t *testing.T) {
	var wire bytes.Buffer
	err := streamUpload(protocol.NewEncoder(&wire), "cmd", failingReader{})
	if !errorsIsUnexpectedEOF(err) {
		t.Fatalf("err=%v", err)
	}
	var msg protocol.Message
	if err := protocol.NewDecoder(&wire).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "transfer.abort" || !strings.Contains(msg.Error, "unexpected EOF") {
		t.Fatalf("msg=%#v", msg)
	}
}

func errorsIsUnexpectedEOF(err error) bool {
	return err == io.ErrUnexpectedEOF
}
