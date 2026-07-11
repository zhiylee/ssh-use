package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agent-ssh/internal/paths"
)

const (
	ModeAuto      = "auto"
	ModeSensitive = "sensitive"
	ModeApproval  = "approval"
)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

type Config struct {
	Defaults DefaultsConfig  `yaml:"defaults"`
	Hosts    map[string]Host `yaml:"hosts"`
	Policy   PolicyConfig    `yaml:"policy"`
	Audit    AuditConfig     `yaml:"audit"`
	Approval ApprovalConfig  `yaml:"approval"`
	TUI      TUIConfig       `yaml:"tui"`
}

type DefaultsConfig struct {
	User           string   `yaml:"user"`
	Port           int      `yaml:"port"`
	Key            string   `yaml:"key"`
	IdleTimeout    Duration `yaml:"idle_timeout"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	CommandTimeout Duration `yaml:"command_timeout"`
}

type Host struct {
	Addr string   `yaml:"addr"`
	User string   `yaml:"user"`
	Port int      `yaml:"port"`
	Key  string   `yaml:"key"`
	Tags []string `yaml:"tags"`
}

type PolicyConfig struct {
	Mode          string       `yaml:"mode"`
	DefaultAction string       `yaml:"default_action"`
	BuiltinRules  *bool        `yaml:"builtin_rules"`
	Rules         []RuleConfig `yaml:"rules"`
}

type RuleConfig struct {
	Name     string   `yaml:"name"`
	Action   string   `yaml:"action"`
	Risk     string   `yaml:"risk"`
	Patterns []string `yaml:"patterns"`
}

type AuditConfig struct {
	Enabled        *bool  `yaml:"enabled"`
	StoreOutput    string `yaml:"store_output"`
	MaxOutputBytes int    `yaml:"max_output_bytes"`
	RetentionDays  int    `yaml:"retention_days"`
	RedactSecrets  *bool  `yaml:"redact_secrets"`
}

type ApprovalConfig struct {
	Timeout      Duration `yaml:"timeout"`
	ConfirmRisks []string `yaml:"confirm_risks"`
}

type TUIConfig struct {
	Theme         string `yaml:"theme"`
	FocusPending  *bool  `yaml:"focus_pending"`
	BellOnPending bool   `yaml:"bell_on_pending"`
}

func Load() (*Config, error) {
	cfg := Default()
	path := paths.ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return cfg, nil
}

func Save(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	path := paths.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func ValidMode(mode string) bool {
	switch mode {
	case ModeAuto, ModeSensitive, ModeApproval:
		return true
	default:
		return false
	}
}

func Default() *Config {
	trueValue := true
	return &Config{
		Defaults: DefaultsConfig{
			User:           "root",
			Port:           22,
			Key:            "~/.ssh/id_ed25519_agent_ssh",
			IdleTimeout:    Duration{Duration: 10 * time.Minute},
			ConnectTimeout: Duration{Duration: 10 * time.Second},
			CommandTimeout: Duration{Duration: 5 * time.Minute},
		},
		Hosts: map[string]Host{},
		Policy: PolicyConfig{
			Mode:          ModeSensitive,
			DefaultAction: "allow",
			BuiltinRules:  &trueValue,
		},
		Audit: AuditConfig{
			Enabled:        &trueValue,
			StoreOutput:    "summary",
			MaxOutputBytes: 262144,
			RetentionDays:  14,
			RedactSecrets:  &trueValue,
		},
		Approval: ApprovalConfig{
			Timeout:      Duration{Duration: 30 * time.Minute},
			ConfirmRisks: []string{"high", "critical"},
		},
		TUI: TUIConfig{
			Theme:        "dark",
			FocusPending: &trueValue,
		},
	}
}

func (c *Config) ApplyDefaults() {
	defaults := Default()
	if c.Defaults.User == "" {
		c.Defaults.User = defaults.Defaults.User
	}
	if c.Defaults.Port == 0 {
		c.Defaults.Port = defaults.Defaults.Port
	}
	if c.Defaults.Key == "" {
		c.Defaults.Key = defaults.Defaults.Key
	}
	if c.Defaults.IdleTimeout.Duration == 0 {
		c.Defaults.IdleTimeout = defaults.Defaults.IdleTimeout
	}
	if c.Defaults.ConnectTimeout.Duration == 0 {
		c.Defaults.ConnectTimeout = defaults.Defaults.ConnectTimeout
	}
	if c.Defaults.CommandTimeout.Duration == 0 {
		c.Defaults.CommandTimeout = defaults.Defaults.CommandTimeout
	}
	if c.Hosts == nil {
		c.Hosts = map[string]Host{}
	}
	if c.Policy.Mode == "" {
		c.Policy.Mode = defaults.Policy.Mode
	}
	if c.Policy.DefaultAction == "" {
		c.Policy.DefaultAction = defaults.Policy.DefaultAction
	}
	if c.Policy.BuiltinRules == nil {
		c.Policy.BuiltinRules = defaults.Policy.BuiltinRules
	}
	if c.Audit.Enabled == nil {
		c.Audit.Enabled = defaults.Audit.Enabled
	}
	if c.Audit.StoreOutput == "" {
		c.Audit.StoreOutput = defaults.Audit.StoreOutput
	}
	if c.Audit.MaxOutputBytes == 0 {
		c.Audit.MaxOutputBytes = defaults.Audit.MaxOutputBytes
	}
	if c.Audit.RetentionDays == 0 {
		c.Audit.RetentionDays = defaults.Audit.RetentionDays
	}
	if c.Audit.RedactSecrets == nil {
		c.Audit.RedactSecrets = defaults.Audit.RedactSecrets
	}
	if c.Approval.Timeout.Duration == 0 {
		c.Approval.Timeout = defaults.Approval.Timeout
	}
	if c.Approval.ConfirmRisks == nil {
		c.Approval.ConfirmRisks = append([]string(nil), defaults.Approval.ConfirmRisks...)
	}
	if c.TUI.Theme == "" {
		c.TUI.Theme = defaults.TUI.Theme
	}
	if c.TUI.FocusPending == nil {
		c.TUI.FocusPending = defaults.TUI.FocusPending
	}
}

func (c *Config) Validate() error {
	for _, risk := range c.Approval.ConfirmRisks {
		switch risk {
		case "low", "medium", "high", "critical":
		default:
			return fmt.Errorf("approval.confirm_risks contains invalid risk %q; valid risks are low, medium, high, critical", risk)
		}
	}
	return nil
}

func (c *Config) BuiltinRulesEnabled() bool {
	return c.Policy.BuiltinRules == nil || *c.Policy.BuiltinRules
}

func (c *Config) AuditEnabled() bool {
	return c.Audit.Enabled == nil || *c.Audit.Enabled
}

func (c *Config) RedactSecrets() bool {
	return c.Audit.RedactSecrets == nil || *c.Audit.RedactSecrets
}

func (c *Config) ResolveHost(name string) (Host, error) {
	host, ok := c.Hosts[name]
	if !ok {
		host = Host{Addr: name}
	}
	if host.Addr == "" {
		return Host{}, fmt.Errorf("host %q has no addr", name)
	}
	if host.User == "" {
		host.User = c.Defaults.User
	}
	if host.Port == 0 {
		host.Port = c.Defaults.Port
	}
	if host.Key == "" {
		host.Key = c.Defaults.Key
	}
	host.Key = paths.ExpandHome(host.Key)
	return host, nil
}

func (h Host) IsProd(name string) bool {
	if strings.Contains(strings.ToLower(name), "prod") {
		return true
	}
	for _, tag := range h.Tags {
		if strings.EqualFold(tag, "prod") || strings.EqualFold(tag, "production") {
			return true
		}
	}
	return false
}
