package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agent-ssh/internal/paths"
)

var ErrConfigChanged = errors.New("configuration file changed")

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
	Addr           string        `yaml:"addr"`
	User           string        `yaml:"user"`
	Port           int           `yaml:"port"`
	Key            string        `yaml:"key"`
	Tags           []string      `yaml:"tags"`
	ConnectTimeout time.Duration `yaml:"-"`
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
	path := paths.ConfigPath()
	cfg, err := LoadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, err
	}
	return cfg, nil
}

func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return cfg, nil
}

func Parse(data []byte) (*Config, error) {
	cfg := Default()
	if len(bytes.TrimSpace(data)) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil {
			return nil, err
		}
		var extra any
		if err := decoder.Decode(&extra); err == nil {
			return nil, fmt.Errorf("configuration must contain exactly one YAML document")
		} else if !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Save(cfg *Config) error {
	return save(cfg, nil)
}

func SaveIfUnchanged(cfg *Config, expected []byte) error {
	return save(cfg, func(path string) error {
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, expected) {
			return ErrConfigChanged
		}
		return nil
	})
}

func save(cfg *Config, verify func(string) error) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	path, err := configWritePath(paths.ConfigPath())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.yaml-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if verify != nil {
		if err := verify(path); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func configWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	return filepath.EvalSymlinks(path)
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
	builtinRules := true
	auditEnabled := true
	redactSecrets := true
	focusPending := true
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
			BuiltinRules:  &builtinRules,
		},
		Audit: AuditConfig{
			Enabled:        &auditEnabled,
			StoreOutput:    "summary",
			MaxOutputBytes: 262144,
			RetentionDays:  14,
			RedactSecrets:  &redactSecrets,
		},
		Approval: ApprovalConfig{
			Timeout:      Duration{Duration: 30 * time.Minute},
			ConfirmRisks: []string{"high", "critical"},
		},
		TUI: TUIConfig{
			Theme:        "dark",
			FocusPending: &focusPending,
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

func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	clone := *c
	clone.Hosts = make(map[string]Host, len(c.Hosts))
	for name, host := range c.Hosts {
		host.Tags = append([]string(nil), host.Tags...)
		clone.Hosts[name] = host
	}
	clone.Policy.Rules = make([]RuleConfig, len(c.Policy.Rules))
	for i, rule := range c.Policy.Rules {
		rule.Patterns = append([]string(nil), rule.Patterns...)
		clone.Policy.Rules[i] = rule
	}
	if c.Approval.ConfirmRisks != nil {
		clone.Approval.ConfirmRisks = append([]string{}, c.Approval.ConfirmRisks...)
	}
	clone.Policy.BuiltinRules = cloneBool(c.Policy.BuiltinRules)
	clone.Audit.Enabled = cloneBool(c.Audit.Enabled)
	clone.Audit.RedactSecrets = cloneBool(c.Audit.RedactSecrets)
	clone.TUI.FocusPending = cloneBool(c.TUI.FocusPending)
	return &clone
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func (c *Config) Validate() error {
	if !ValidMode(c.Policy.Mode) {
		return fmt.Errorf("policy.mode %q is invalid; valid modes are auto, sensitive, approval", c.Policy.Mode)
	}
	switch c.Policy.DefaultAction {
	case "allow", "approve", "block":
	default:
		return fmt.Errorf("policy.default_action %q is invalid; valid actions are allow, approve, block", c.Policy.DefaultAction)
	}
	if c.Defaults.Port < 1 || c.Defaults.Port > 65535 {
		return fmt.Errorf("defaults.port must be between 1 and 65535")
	}
	if c.Defaults.IdleTimeout.Duration <= 0 || c.Defaults.ConnectTimeout.Duration <= 0 || c.Defaults.CommandTimeout.Duration <= 0 {
		return fmt.Errorf("default timeouts must be positive")
	}
	if c.Approval.Timeout.Duration <= 0 {
		return fmt.Errorf("approval.timeout must be positive")
	}
	if c.Audit.MaxOutputBytes < 0 {
		return fmt.Errorf("audit.max_output_bytes must not be negative")
	}
	for name, host := range c.Hosts {
		if strings.TrimSpace(host.Addr) == "" {
			return fmt.Errorf("host %q has no addr", name)
		}
		if host.Port < 0 || host.Port > 65535 {
			return fmt.Errorf("host %q port must be 0 or between 1 and 65535", name)
		}
	}
	for _, rule := range c.Policy.Rules {
		if rule.Action != "" {
			switch rule.Action {
			case "allow", "approve", "block":
			default:
				return fmt.Errorf("policy rule %q has invalid action %q", rule.Name, rule.Action)
			}
		}
		if rule.Risk != "" {
			switch rule.Risk {
			case "low", "medium", "high", "critical":
			default:
				return fmt.Errorf("policy rule %q has invalid risk %q", rule.Name, rule.Risk)
			}
		}
	}
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
	host.ConnectTimeout = c.Defaults.ConnectTimeout.Duration
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
