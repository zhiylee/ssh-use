package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zhiylee/ssh-use/skills"
)

func RunSkill(args []string) int {
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprintln(stderr, "usage: ssh-use skill install --agent <claude|opencode|codex> [--scope user|project] [--force]")
		fmt.Fprintln(stderr, "       ssh-use skill install --dir <skills-directory> [--force]")
		return 2
	}
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	agent := flags.String("agent", "", "agent: claude, opencode, or codex")
	scope := flags.String("scope", "user", "install for user or current project")
	dir := flags.String("dir", "", "custom parent skills directory (exclusive with agent/scope)")
	force := flags.Bool("force", false, "replace an existing skill directory, including local edits")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	scopeSet := false
	flags.Visit(func(f *flag.Flag) { scopeSet = scopeSet || f.Name == "scope" })
	if flags.NArg() != 0 || (*dir != "" && (*agent != "" || scopeSet)) || (*dir == "" && *agent == "") {
		fmt.Fprintln(stderr, "ssh-use: choose --agent with optional --scope, or --dir alone")
		return 2
	}
	root := *dir
	if root == "" {
		var err error
		root, err = skillDirectory(*agent, *scope)
		if err != nil {
			fmt.Fprintf(stderr, "ssh-use: %v\n", err)
			return 2
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-use: skill directory: %v\n", err)
		return 1
	}
	if err := installSkill(root, *force); err != nil {
		fmt.Fprintf(stderr, "ssh-use: install skill: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "installed ssh-use skill: %s\n", filepath.Join(root, "ssh-use", "SKILL.md"))
	return 0
}

func skillDirectory(agent, scope string) (string, error) {
	if agent != "claude" && agent != "opencode" && agent != "codex" {
		return "", fmt.Errorf("unknown agent %q; choose claude, opencode, or codex", agent)
	}
	if scope != "user" && scope != "project" {
		return "", fmt.Errorf("unknown scope %q; choose user or project", scope)
	}
	if scope == "project" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		folder := map[string]string{"claude": ".claude", "opencode": ".opencode", "codex": ".agents"}[agent]
		return filepath.Join(cwd, folder, "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch agent {
	case "claude":
		return filepath.Join(home, ".claude", "skills"), nil
	case "opencode":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, "opencode", "skills"), nil
	default:
		return filepath.Join(home, ".agents", "skills"), nil
	}
}

func installSkill(root string, force bool) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Serialize installers sharing this destination, including forced upgrades.
	lockPath := filepath.Join(root, ".ssh-use-install.lock")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acquire install lock %s: %w", lockPath, err)
	}
	_ = lock.Close()
	defer os.Remove(lockPath)

	dest := filepath.Join(root, "ssh-use")
	info, err := os.Lstat(dest)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if exists {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a regular directory; refusing to replace it", dest)
		}
		if !force {
			return fmt.Errorf("%s already exists; use --force to replace this skill including local edits", dest)
		}
	}
	stage, err := os.MkdirTemp(root, ".ssh-use-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	files := skills.Files()
	if err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		path := filepath.Join(stage, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(path, 0o755)
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o644)
	}); err != nil {
		return err
	}
	backup := ""
	if exists {
		backup, err = os.MkdirTemp(root, ".ssh-use-backup-")
		if err != nil {
			return err
		}
		if err := os.Rename(dest, filepath.Join(backup, "ssh-use")); err != nil {
			_ = os.Remove(backup)
			return err
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(filepath.Join(backup, "ssh-use"), dest); restoreErr != nil {
				return fmt.Errorf("publish skill: %w; restore failed: %v; previous skill preserved in %s", err, restoreErr, backup)
			}
			_ = os.Remove(backup)
		}
		return err
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("skill installed, but previous skill cleanup failed at %s: %w", backup, err)
		}
	}
	return nil
}
