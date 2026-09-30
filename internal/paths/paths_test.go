package paths

import (
	"path/filepath"
	"testing"
)

func TestEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_SSH_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("AGENT_SSH_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("AGENT_SSH_CONFIG_PATH", filepath.Join(dir, "config.yaml"))
	if RuntimeDir() != filepath.Join(dir, "run") {
		t.Fatalf("runtime dir = %q", RuntimeDir())
	}
	if SocketPath() != filepath.Join(dir, "run", "agent-ssh.sock") {
		t.Fatalf("socket path = %q", SocketPath())
	}
	if DataDir() != filepath.Join(dir, "data") || DBPath() != filepath.Join(dir, "data", "agent-ssh.db") {
		t.Fatalf("data paths = %q %q", DataDir(), DBPath())
	}
	if ConfigPath() != filepath.Join(dir, "config.yaml") {
		t.Fatalf("config path = %q", ConfigPath())
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	if ExpandHome("~/id") != filepath.Join("/home/tester", "id") {
		t.Fatalf("ExpandHome = %q", ExpandHome("~/id"))
	}
	if ExpandHome("/abs") != "/abs" {
		t.Fatal("absolute path changed")
	}
}

func TestXDGPathsAndLogPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_SSH_RUNTIME_DIR", "")
	t.Setenv("AGENT_SSH_DATA_DIR", "")
	t.Setenv("AGENT_SSH_CONFIG_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "runtime"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data-home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config-home"))
	if RuntimeDir() != filepath.Join(dir, "runtime", "agent-ssh") {
		t.Fatalf("runtime=%q", RuntimeDir())
	}
	if LogPath() != filepath.Join(dir, "runtime", "agent-ssh", "daemon.log") {
		t.Fatalf("log=%q", LogPath())
	}
	if DataDir() != filepath.Join(dir, "data-home", "agent-ssh") {
		t.Fatalf("data=%q", DataDir())
	}
	if ConfigPath() != filepath.Join(dir, "config-home", "agent-ssh", "config.yaml") {
		t.Fatalf("config=%q", ConfigPath())
	}
}

func TestRuntimeDirFallback(t *testing.T) {
	if got := fallbackRuntimeDir(123, "/tmp/test", true); got != "/run/user/123/agent-ssh" {
		t.Fatalf("system user runtime=%q", got)
	}
	if got := fallbackRuntimeDir(123, "/tmp/test", false); got != filepath.Join("/tmp/test", "agent-ssh-123") {
		t.Fatalf("temporary runtime=%q", got)
	}
}

func TestHomeFallbackPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_SSH_RUNTIME_DIR", "")
	t.Setenv("AGENT_SSH_DATA_DIR", "")
	t.Setenv("AGENT_SSH_CONFIG_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", dir)
	if ConfigPath() != filepath.Join(dir, ".config", "agent-ssh", "config.yaml") {
		t.Fatalf("config=%q", ConfigPath())
	}
	if DataDir() != filepath.Join(dir, ".local", "share", "agent-ssh") {
		t.Fatalf("data=%q", DataDir())
	}
	if ExpandHome("~") != dir {
		t.Fatalf("home=%q", ExpandHome("~"))
	}
	if ExpandHome("~other/path") != "~other/path" {
		t.Fatalf("tilde user changed")
	}
}
