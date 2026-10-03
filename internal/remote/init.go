package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Init writes a private, self-contained server directory and two portable client
// bundles. Existing directories are refused so credentials cannot be overwritten.
func Init(dir, host, port string) error {
	if host == "" {
		return fmt.Errorf("--host is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "ssh-use " + host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		cert.IPAddresses = []net.IP{ip}
	} else {
		cert.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for file, data := range map[string][]byte{"tls.crt": certPEM, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			return err
		}
	}
	auth := AuthConfig{Clients: map[string]Credential{}}
	for _, role := range []string{"admin", "agent"} {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		value := hex.EncodeToString(token)
		digest := sha256.Sum256([]byte(value))
		auth.Clients[role] = Credential{Role: role, TokenSHA256: hex.EncodeToString(digest[:])}
		bundle := filepath.Join(dir, role)
		if err := os.Mkdir(bundle, 0700); err != nil {
			return err
		}
		cfg, err := yaml.Marshal(ClientConfig{Endpoint: net.JoinHostPort(host, port), CAFile: "ca.pem", TokenFile: "token"})
		if err != nil {
			return err
		}
		for file, data := range map[string][]byte{"client.yaml": cfg, "ca.pem": certPEM, "token": []byte(value + "\n")} {
			if err := os.WriteFile(filepath.Join(bundle, file), data, 0600); err != nil {
				return err
			}
		}
	}
	data, err := yaml.Marshal(auth)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "auth.yaml"), data, 0600)
}
