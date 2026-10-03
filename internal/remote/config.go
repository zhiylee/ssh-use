// Package remote contains the TLS transport bootstrap configuration. It is
// intentionally separate from the server's SSH hosts and policy configuration.
package remote

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhiylee/ssh-use/internal/paths"
	"gopkg.in/yaml.v3"
)

const Version = 2

type ClientConfig struct {
	Endpoint  string `yaml:"endpoint"`
	CAFile    string `yaml:"ca_file"`
	TokenFile string `yaml:"token_file"`
}

type Credential struct {
	TokenSHA256 string `yaml:"token_sha256"`
	Role        string `yaml:"role"`
}

type AuthConfig struct {
	Clients map[string]Credential `yaml:"clients"`
}

func ReadYAML(file string, dst any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected one YAML document", file)
	}
	return nil
}

func LoadClient() (ClientConfig, error) {
	var cfg ClientConfig
	file := os.Getenv("SSH_USE_CLIENT_CONFIG")
	explicit := file != ""
	if !explicit {
		file = filepath.Join(filepath.Dir(paths.ConfigPath()), "client.yaml")
	}
	file = paths.ExpandHome(file)
	if err := ReadYAML(file, &cfg); err != nil && (explicit || !os.IsNotExist(err)) {
		return cfg, err
	}
	for _, item := range []struct {
		env string
		dst *string
	}{
		{"SSH_USE_SERVER", &cfg.Endpoint}, {"SSH_USE_CA_FILE", &cfg.CAFile}, {"SSH_USE_TOKEN_FILE", &cfg.TokenFile},
	} {
		if v := os.Getenv(item.env); v != "" {
			*item.dst = v
		}
	}
	if cfg.Endpoint == "" {
		if explicit || cfg.CAFile != "" || cfg.TokenFile != "" {
			return cfg, fmt.Errorf("remote endpoint is required")
		}
		return cfg, nil
	}
	if _, _, err := net.SplitHostPort(cfg.Endpoint); err != nil {
		return cfg, fmt.Errorf("endpoint must be host:port: %w", err)
	}
	if cfg.TokenFile == "" {
		return cfg, fmt.Errorf("remote token_file is required")
	}
	for _, item := range []struct {
		env string
		dst *string
	}{{"SSH_USE_CA_FILE", &cfg.CAFile}, {"SSH_USE_TOKEN_FILE", &cfg.TokenFile}} {
		if *item.dst == "" {
			continue
		}
		*item.dst = paths.ExpandHome(*item.dst)
		if !filepath.IsAbs(*item.dst) && os.Getenv(item.env) == "" {
			*item.dst = filepath.Join(filepath.Dir(file), *item.dst)
		}
	}
	return cfg, nil
}

func LoadAuth(file string) (*AuthConfig, error) {
	var cfg AuthConfig
	if err := ReadYAML(file, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Clients) == 0 {
		return nil, fmt.Errorf("auth file must contain at least one client")
	}
	seen := map[string]bool{}
	for name, cred := range cfg.Clients {
		digest, err := hex.DecodeString(cred.TokenSHA256)
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n\x00") || err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("invalid client identity or token hash for %q", name)
		}
		if cred.Role != "admin" && cred.Role != "agent" {
			return nil, fmt.Errorf("invalid role for %q: use admin or agent", name)
		}
		key := string(digest)
		if seen[key] {
			return nil, fmt.Errorf("client tokens must be unique")
		}
		seen[key] = true
	}
	return &cfg, nil
}

func (a *AuthConfig) Authenticate(token string) (name, role string) {
	digest := sha256.Sum256([]byte(token))
	for id, cred := range a.Clients {
		expected, _ := hex.DecodeString(cred.TokenSHA256)
		if subtle.ConstantTimeCompare(digest[:], expected) == 1 {
			return id, cred.Role
		}
	}
	return "", ""
}
