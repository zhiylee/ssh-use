package daemon

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/paths"
	"github.com/zhiylee/ssh-use/internal/policy"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"gopkg.in/yaml.v3"
)

var hostNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func configRevision(cfg *config.Config) string {
	data, _ := yaml.Marshal(cfg)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Server) manageHosts(req protocol.Message) protocol.Message {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	fail := func(code string, err error) protocol.Message {
		return protocol.Message{Type: "error", Error: err.Error(), SSHUseErrorCode: code}
	}
	// Read the latest file while holding the same lock as reload and policy edits.
	data, err := os.ReadFile(paths.ConfigPath())
	exists := err == nil
	var cfg *config.Config
	if exists {
		cfg, err = config.Parse(data)
	} else if os.IsNotExist(err) {
		current, _ := s.currentConfig()
		cfg = current.Clone()
		err = nil
	}
	if err != nil {
		return fail("config_error", err)
	}
	revision := configRevision(cfg)
	if req.Type == "hosts.list" {
		return protocol.Message{Type: "hosts", OK: true, Hosts: cfg.Clone().Hosts, ConfigRevision: revision}
	}
	host, found := cfg.Hosts[req.Host]
	if req.Type == "hosts.get" {
		if !found {
			return fail("host_not_found", fmt.Errorf("unknown host %q", req.Host))
		}
		return protocol.Message{Type: "host", OK: true, Host: req.Host, HostConfig: &host, ConfigRevision: revision}
	}
	if !hostNamePattern.MatchString(req.Host) {
		return fail("invalid_host", fmt.Errorf("host name must use 1-128 letters, digits, dots, underscores or hyphens"))
	}
	if req.ConfigRevision == "" || req.ConfigRevision != revision {
		return fail("config_conflict", fmt.Errorf("configuration changed; refresh and retry"))
	}
	switch req.Type {
	case "hosts.add":
		if found {
			return fail("host_exists", fmt.Errorf("host %q already exists", req.Host))
		}
		if req.HostConfig == nil {
			return fail("invalid_host", fmt.Errorf("host_config is required"))
		}
		cfg.Hosts[req.Host] = *req.HostConfig
	case "hosts.update":
		if !found {
			return fail("host_not_found", fmt.Errorf("unknown host %q", req.Host))
		}
		p := req.HostPatch
		if p == nil {
			return fail("invalid_host", fmt.Errorf("host_patch is required"))
		}
		if p.Addr != nil {
			host.Addr = *p.Addr
		}
		if p.User != nil {
			host.User = *p.User
		}
		if p.Port != nil {
			host.Port = *p.Port
		}
		if p.Key != nil {
			host.Key = *p.Key
		}
		if p.Tags != nil {
			host.Tags = append([]string{}, (*p.Tags)...)
		}
		cfg.Hosts[req.Host] = host
	case "hosts.delete":
		if !found {
			return fail("host_not_found", fmt.Errorf("unknown host %q", req.Host))
		}
		delete(cfg.Hosts, req.Host)
	}
	if err := cfg.Validate(); err != nil {
		return fail("config_error", err)
	}
	engine, err := policy.New(cfg.Policy)
	if err != nil {
		return fail("config_error", err)
	}
	current, _ := s.currentConfig()
	if cfg.AuditEnabled() != current.AuditEnabled() {
		return fail("config_error", fmt.Errorf("audit.enabled changes require a restart"))
	}
	if exists {
		err = config.SaveIfUnchanged(cfg, data)
	} else {
		err = config.Save(cfg)
	}
	if err != nil {
		return fail("config_conflict", err)
	}
	s.publishConfig(cfg, engine)
	_, _, generation := s.currentConfigSnapshot()
	s.broadcast(protocol.Message{Type: "config.reloaded", OK: true, Mode: engine.Mode(), RuntimeSettings: runtimeSettings(cfg, engine, generation)})
	return protocol.Message{Type: "ack", OK: true, Host: req.Host, ConfigRevision: configRevision(cfg)}
}
