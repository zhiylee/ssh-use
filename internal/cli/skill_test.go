package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhiylee/ssh-use/skills"
)

func TestSkillInstallCopiesBundleAndProtectsExistingFiles(t *testing.T) {
	var out, errOut bytes.Buffer
	defer stubCLI(t, &out, &errOut)()
	root := filepath.Join(t.TempDir(), "skills with spaces")
	if code := RunSkill([]string{"install", "--dir", root}); code != 0 {
		t.Fatalf("install: code=%d stderr=%s", code, errOut.String())
	}
	assertInstalledSkill(t, root)
	file := filepath.Join(root, "ssh-use", "SKILL.md")
	if err := os.WriteFile(file, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := RunSkill([]string{"install", "--dir", root}); code != 1 {
		t.Fatalf("overwrite without force: code=%d", code)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "local edit" {
		t.Fatal("existing skill was changed")
	}
	extra := filepath.Join(root, "ssh-use", "local-extra.txt")
	if err := os.WriteFile(extra, []byte("local resource"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "another-skill")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := RunSkill([]string{"install", "--dir", root, "--force"}); code != 0 {
		t.Fatalf("upgrade: code=%d stderr=%s", code, errOut.String())
	}
	assertInstalledSkill(t, root)
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatal("force did not replace the complete skill directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("installer touched siblings or left temporary files: %v %v", entries, err)
	}
}

func TestSkillInstallAgentLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	for _, tc := range []struct{ agent, scope, root string }{
		{"claude", "user", filepath.Join(home, ".claude", "skills")},
		{"opencode", "user", filepath.Join(home, ".config", "opencode", "skills")},
		{"codex", "user", filepath.Join(home, ".agents", "skills")},
		{"claude", "project", filepath.Join(cwd, ".claude", "skills")},
		{"opencode", "project", filepath.Join(cwd, ".opencode", "skills")},
		{"codex", "project", filepath.Join(cwd, ".agents", "skills")},
	} {
		t.Run(tc.agent+"/"+tc.scope, func(t *testing.T) {
			var out, errOut bytes.Buffer
			defer stubCLI(t, &out, &errOut)()
			if code := RunSkill([]string{"install", "--agent", tc.agent, "--scope", tc.scope}); code != 0 {
				t.Fatalf("install: code=%d stderr=%s", code, errOut.String())
			}
			assertInstalledSkill(t, tc.root)
		})
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	root, err := skillDirectory("opencode", "user")
	if err != nil || root != filepath.Join(home, "xdg", "opencode", "skills") {
		t.Fatalf("XDG location: %q %v", root, err)
	}
}

func TestSkillInstallRejectsInvalidArgumentsWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	custom := filepath.Join(home, "custom")
	for _, args := range [][]string{
		nil, {"unknown"}, {"install"}, {"install", "--agent", "unknown"},
		{"install", "--agent", "claude", "--scope", "unknown"},
		{"install", "--dir", custom, "--agent", "claude"},
		{"install", "--dir", custom, "--scope", "user"},
		{"install", "--dir", custom, "extra"}, {"install", "--bogus"},
	} {
		var out, errOut bytes.Buffer
		restore := stubCLI(t, &out, &errOut)
		code := RunSkill(args)
		restore()
		if code != 2 {
			t.Fatalf("args=%v: code=%d stderr=%s", args, code, errOut.String())
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("invalid arguments created files")
	}
}

func TestSkillInstallRejectsSymlinkAndBusyDestination(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "ssh-use")); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(root, true); err == nil {
		t.Fatal("replaced a symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote through symlink")
	}
	root = t.TempDir()
	lock := filepath.Join(root, ".ssh-use-install.lock")
	if err := os.WriteFile(lock, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(root, true); err == nil {
		t.Fatal("ignored active installer lock")
	}
	data, _ := os.ReadFile(lock)
	if string(data) != "occupied" {
		t.Fatal("removed another installer's lock")
	}
}

func assertInstalledSkill(t *testing.T, root string) {
	t.Helper()
	files := skills.Files()
	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		want, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(root, "ssh-use", filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("installed %s differs from bundle", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
