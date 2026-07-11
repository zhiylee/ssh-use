package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.Defaults.Key != "~/.ssh/id_ed25519_agent_ssh" {
		t.Fatalf("default key = %q", cfg.Defaults.Key)
	}
	if cfg.Policy.Mode != ModeSensitive {
		t.Fatalf("default mode = %q", cfg.Policy.Mode)
	}
	if cfg.Policy.DefaultAction != "allow" {
		t.Fatalf("default action = %q", cfg.Policy.DefaultAction)
	}
	if !cfg.AuditEnabled() || !cfg.RedactSecrets() || !cfg.BuiltinRulesEnabled() {
		t.Fatal("expected enabled defaults")
	}
	if !slices.Equal(cfg.Approval.ConfirmRisks, []string{"high", "critical"}) {
		t.Fatalf("default approval confirm risks = %#v", cfg.Approval.ConfirmRisks)
	}
}

func TestLoadMissingUsesDefaults(t *testing.T) {
	t.Setenv("AGENT_SSH_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.User != "root" || cfg.Defaults.Port != 22 || cfg.Policy.Mode != ModeSensitive {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
}

func TestLoadAppliesDefaultsAndResolveHost(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	data := []byte(`
defaults:
  user: ubuntu
  key: ` + key + `
hosts:
  prod1:
    addr: 10.0.0.1
policy:
  mode: approval
approval:
  confirm_risks: [medium]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Port != 22 || cfg.Defaults.CommandTimeout.Duration != 5*time.Minute {
		t.Fatalf("defaults not applied: %#v", cfg.Defaults)
	}
	if !slices.Equal(cfg.Approval.ConfirmRisks, []string{"medium"}) {
		t.Fatalf("approval confirm risks = %#v", cfg.Approval.ConfirmRisks)
	}
	host, err := cfg.ResolveHost("prod1")
	if err != nil {
		t.Fatal(err)
	}
	if host.User != "ubuntu" || host.Port != 22 || host.Key != key {
		t.Fatalf("resolved host = %#v", host)
	}
	unknown, err := cfg.ResolveHost("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Addr != "example.com" {
		t.Fatalf("unknown host fallback = %#v", unknown)
	}
}

func TestSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	cfg := Default()
	cfg.Policy.Mode = ModeAuto
	cfg.Hosts["h"] = Host{Addr: "127.0.0.1", User: "me"}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Policy.Mode != ModeAuto || loaded.Hosts["h"].Addr != "127.0.0.1" {
		t.Fatalf("loaded config = %#v", loaded)
	}
}

func TestValidModeAndProdDetection(t *testing.T) {
	if !ValidMode(ModeAuto) || !ValidMode(ModeSensitive) || !ValidMode(ModeApproval) || ValidMode("bad") {
		t.Fatal("valid mode check failed")
	}
	if !((Host{}).IsProd("prod-api")) {
		t.Fatal("prod name not detected")
	}
	if !(Host{Tags: []string{"production"}}).IsProd("api") {
		t.Fatal("prod tag not detected")
	}
	if (Host{Tags: []string{"dev"}}).IsProd("api") {
		t.Fatal("dev host detected as prod")
	}
}

func TestApplyDefaultsFillsAllZeroValues(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if cfg.Defaults.User != "root" || cfg.Defaults.Port != 22 || cfg.Defaults.Key == "" {
		t.Fatalf("defaults=%#v", cfg.Defaults)
	}
	if cfg.Defaults.IdleTimeout.Duration == 0 || cfg.Defaults.ConnectTimeout.Duration == 0 || cfg.Defaults.CommandTimeout.Duration == 0 {
		t.Fatalf("durations=%#v", cfg.Defaults)
	}
	if cfg.Hosts == nil || cfg.Policy.Mode != ModeSensitive || cfg.Policy.DefaultAction != "allow" || cfg.Policy.BuiltinRules == nil {
		t.Fatalf("policy/hosts=%#v %#v", cfg.Hosts, cfg.Policy)
	}
	if cfg.Audit.Enabled == nil || cfg.Audit.StoreOutput != "summary" || cfg.Audit.MaxOutputBytes == 0 || cfg.Audit.RetentionDays == 0 || cfg.Audit.RedactSecrets == nil {
		t.Fatalf("audit=%#v", cfg.Audit)
	}
	if cfg.Approval.Timeout.Duration == 0 || !slices.Equal(cfg.Approval.ConfirmRisks, []string{"high", "critical"}) || cfg.TUI.Theme != "dark" || cfg.TUI.FocusPending == nil {
		t.Fatalf("approval/tui=%#v %#v", cfg.Approval, cfg.TUI)
	}
}

func TestExplicitEmptyApprovalConfirmRisks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte("approval:\n  confirm_risks: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Approval.ConfirmRisks == nil || len(cfg.Approval.ConfirmRisks) != 0 {
		t.Fatalf("explicit empty confirm risks = %#v", cfg.Approval.ConfirmRisks)
	}
}

func TestInvalidApprovalConfirmRisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte("approval:\n  confirm_risks: [urgent]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "invalid risk") {
		t.Fatalf("invalid risk error = %v", err)
	}
}

func TestDurationInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	if err := os.WriteFile(path, []byte("defaults:\n  command_timeout: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid duration error")
	}
}
