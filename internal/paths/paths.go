package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func RuntimeDir() string {
	if dir := os.Getenv("SSH_USE_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ssh-use")
	}
	uid := os.Getuid()
	userRuntime := filepath.Join("/run/user", strconv.Itoa(uid))
	info, err := os.Stat(userRuntime)
	return fallbackRuntimeDir(uid, os.TempDir(), err == nil && info.IsDir())
}

func fallbackRuntimeDir(uid int, tempDir string, hasUserRuntime bool) string {
	if hasUserRuntime {
		return filepath.Join("/run/user", strconv.Itoa(uid), "ssh-use")
	}
	return filepath.Join(tempDir, fmt.Sprintf("ssh-use-%d", uid))
}

func SocketPath() string {
	return filepath.Join(RuntimeDir(), "ssh-use.sock")
}

func LogPath() string {
	return filepath.Join(RuntimeDir(), "daemon.log")
}

func ConfigPath() string {
	if path := os.Getenv("SSH_USE_CONFIG_PATH"); path != "" {
		return path
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ssh-use", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "ssh-use", "config.yaml")
	}
	return filepath.Join(home, ".config", "ssh-use", "config.yaml")
}

func DataDir() string {
	if dir := os.Getenv("SSH_USE_DATA_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "ssh-use")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".local", "share", "ssh-use")
	}
	return filepath.Join(home, ".local", "share", "ssh-use")
}

func DBPath() string {
	return filepath.Join(DataDir(), "ssh-use.db")
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
