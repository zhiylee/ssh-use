package redact

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	in := "password=abc token=def secret=ghi api_key=jkl Authorization: Bearer bearer AWS_SECRET_ACCESS_KEY=aws"
	out := Redact(in)
	for _, secret := range []string{"abc", "def", "ghi", "jkl", "bearer", "aws"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked in %q", secret, out)
		}
	}
	if strings.Count(out, "[REDACTED]") != 6 {
		t.Fatalf("unexpected redaction output: %q", out)
	}
}
