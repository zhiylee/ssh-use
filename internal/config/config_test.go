package config

import (
	"errors"
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

func TestParsedBooleanSettingsAreIndependent(t *testing.T) {
	cfg, err := Parse([]byte("tui:\n  focus_pending: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.FocusPending == nil || *cfg.TUI.FocusPending {
		t.Fatalf("focus_pending=%v", cfg.TUI.FocusPending)
	}
	if !cfg.AuditEnabled() || !cfg.RedactSecrets() || !cfg.BuiltinRulesEnabled() {
		t.Fatalf("unrelated booleans changed: audit=%v redact=%v builtin=%v", cfg.AuditEnabled(), cfg.RedactSecrets(), cfg.BuiltinRulesEnabled())
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

func TestLoadFileMissingIsAnError(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
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

func TestSaveIfUnchangedDoesNotOverwriteConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("AGENT_SSH_CONFIG_PATH", path)
	cfg := Default()
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("policy:\n  mode: approval\n")
	if err := os.WriteFile(path, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Policy.Mode = ModeAuto
	if err := SaveIfUnchanged(cfg, expected); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("save error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, concurrent) {
		t.Fatalf("concurrent edit was overwritten: %q", got)
	}
}

func TestSavePreservesConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed.yaml")
	link := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("policy:\n  mode: sensitive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SSH_CONFIG_PATH", link)
	cfg := Default()
	cfg.Policy.Mode = ModeAuto
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config symlink was replaced")
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Policy.Mode != ModeAuto {
		t.Fatalf("saved mode=%s", loaded.Policy.Mode)
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

func TestCloneIsIndependent(t *testing.T) {
	cfg := Default()
	cfg.Hosts["prod"] = Host{Addr: "old", Tags: []string{"prod"}}
	cfg.Policy.Rules = []RuleConfig{{Name: "rule", Patterns: []string{"old"}}}
	clone := cfg.Clone()
	host := clone.Hosts["prod"]
	host.Addr = "new"
	host.Tags[0] = "dev"
	clone.Hosts["prod"] = host
	clone.Policy.Rules[0].Patterns[0] = "new"
	clone.Approval.ConfirmRisks[0] = "low"
	*clone.Policy.BuiltinRules = false

	if cfg.Hosts["prod"].Addr != "old" || cfg.Hosts["prod"].Tags[0] != "prod" || cfg.Policy.Rules[0].Patterns[0] != "old" || cfg.Approval.ConfirmRisks[0] != "high" || !*cfg.Policy.BuiltinRules {
		t.Fatalf("clone mutated source: %#v", cfg)
	}
}

func TestValidateRejectsInvalidRuntimeSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"mode", func(cfg *Config) { cfg.Policy.Mode = "invalid" }},
		{"default action", func(cfg *Config) { cfg.Policy.DefaultAction = "invalid" }},
		{"default port", func(cfg *Config) { cfg.Defaults.Port = 70000 }},
		{"host address", func(cfg *Config) { cfg.Hosts["bad"] = Host{} }},
		{"rule action", func(cfg *Config) { cfg.Policy.Rules = []RuleConfig{{Name: "bad", Action: "invalid"}} }},
		{"rule risk", func(cfg *Config) { cfg.Policy.Rules = []RuleConfig{{Name: "bad", Risk: "urgent"}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	if _, err := Parse([]byte("hosts:\n  prod:\n    addr: host\n    typo: value\n")); err == nil {
		t.Fatal("expected unknown field error")
	}
	if _, err := Parse([]byte("policy:\n  mode: auto\n---\npolicy:\n  mode: approval\n")); err == nil {
		t.Fatal("expected multiple document error")
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
