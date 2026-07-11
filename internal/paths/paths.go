package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

func RuntimeDir() string {
	if dir := os.Getenv("AGENT_SSH_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agent-ssh")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-ssh-%d", os.Getuid()))
}

func SocketPath() string {
	return filepath.Join(RuntimeDir(), "agent-ssh.sock")
}

func LogPath() string {
	return filepath.Join(RuntimeDir(), "daemon.log")
}

func ConfigPath() string {
	if path := os.Getenv("AGENT_SSH_CONFIG_PATH"); path != "" {
		return path
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "agent-ssh", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "agent-ssh", "config.yaml")
	}
	return filepath.Join(home, ".config", "agent-ssh", "config.yaml")
}

func DataDir() string {
	if dir := os.Getenv("AGENT_SSH_DATA_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "agent-ssh")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".local", "share", "agent-ssh")
	}
	return filepath.Join(home, ".local", "share", "agent-ssh")
}

func DBPath() string {
	return filepath.Join(DataDir(), "agent-ssh.db")
}

func ExpandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if len(path) == 1 {
		return home
	}
	if path[1] == '/' {
		return filepath.Join(home, path[2:])
	}
	return path
}
