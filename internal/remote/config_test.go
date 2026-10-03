package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitPortableConfigAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	if err := Init(dir, "example.test", "7443"); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, "example.test", "7443"); err == nil {
		t.Fatal("overwrote credentials")
	}
	for _, name := range []string{"tls.key", "auth.yaml", "admin/token", "agent/token"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s permissions: %v %v", name, info, err)
		}
	}
	t.Setenv("SSH_USE_CLIENT_CONFIG", filepath.Join(dir, "agent", "client.yaml"))
	for _, env := range []string{"SSH_USE_SERVER", "SSH_USE_CA_FILE", "SSH_USE_TOKEN_FILE"} {
		t.Setenv(env, "")
	}
	cfg, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "example.test:7443" || cfg.TokenFile != filepath.Join(dir, "agent", "token") {
		t.Fatalf("config: %#v", cfg)
	}
	auth, err := LoadAuth(filepath.Join(dir, "auth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	name, role := auth.Authenticate(string(token[:len(token)-1]))
	if name != "agent" || role != "agent" {
		t.Fatalf("auth: %s %s", name, role)
	}
}

func TestInvalidExplicitClientNeverSelectsLocal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv("SSH_USE_CLIENT_CONFIG", file)
	if _, err := LoadClient(); err == nil {
		t.Fatal("missing explicit configuration accepted")
	}
	for _, data := range []string{"{}\n", "endpoint: localhost:7443\n", "endpoint: https://localhost:7443\ntoken_file: token\n", "endpoint: localhost:7443\ntoken_file: token\nunknown: true\n"} {
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadClient(); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestIssueAndRevokeDeviceCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	if err := Init(dir, "127.0.0.1", "7443"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "device")
	if err := EditClient(dir, "laptop", "admin", out, "127.0.0.1:7443", false); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadAuth(filepath.Join(dir, "auth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if auth.Clients["laptop"].Role != "admin" {
		t.Fatal("device not persisted")
	}
	if err := EditClient(dir, "laptop", "agent", out, "127.0.0.1:7443", false); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if err := EditClient(dir, "admin", "", "", "", true); err != nil {
		t.Fatal(err)
	}
	if err := EditClient(dir, "laptop", "", "", "", true); err == nil {
		t.Fatal("last admin removed")
	}
	if err := EditClient(dir, "agent", "", "", "", true); err != nil {
		t.Fatal(err)
	}
	auth, err = LoadAuth(filepath.Join(dir, "auth.yaml"))
	if err != nil || len(auth.Clients) != 1 {
		t.Fatalf("revoke: %#v %v", auth, err)
	}
}
