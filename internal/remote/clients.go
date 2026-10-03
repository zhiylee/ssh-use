package remote

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// EditClient serializes credential changes and atomically replaces the auth file.
// A removed credential stops authenticating new connections immediately.
func EditClient(dir, name, role, out, endpoint string, remove bool) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(name) {
		return fmt.Errorf("invalid client name")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "auth.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	authFile := filepath.Join(dir, "auth.yaml")
	cfg, err := LoadAuth(authFile)
	if err != nil {
		return err
	}
	_, exists := cfg.Clients[name]
	if remove {
		if !exists {
			return fmt.Errorf("unknown client %q", name)
		}
		delete(cfg.Clients, name)
		admins := 0
		for _, cred := range cfg.Clients {
			if cred.Role == "admin" {
				admins++
			}
		}
		if admins == 0 {
			return fmt.Errorf("cannot remove the last admin")
		}
	} else {
		if exists {
			return fmt.Errorf("client %q already exists", name)
		}
		if role != "admin" && role != "agent" {
			return fmt.Errorf("role must be admin or agent")
		}
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return fmt.Errorf("--endpoint must be host:port")
		}
		if out == "" {
			return fmt.Errorf("--out is required")
		}
		cert, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
		if err != nil {
			return err
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		token := hex.EncodeToString(secret)
		digest := sha256.Sum256([]byte(token))
		clientData, err := yaml.Marshal(ClientConfig{Endpoint: endpoint, CAFile: "ca.pem", TokenFile: "token"})
		if err != nil {
			return err
		}
		if err := os.Mkdir(out, 0700); err != nil {
			return err
		}
		for name, data := range map[string][]byte{"client.yaml": clientData, "ca.pem": cert, "token": []byte(token + "\n")} {
			if err := os.WriteFile(filepath.Join(out, name), data, 0600); err != nil {
				return err
			}
		}
		cfg.Clients[name] = Credential{Role: role, TokenSHA256: hex.EncodeToString(digest[:])}
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), authFile)
}
