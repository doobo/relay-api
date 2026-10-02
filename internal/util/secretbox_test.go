package util

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestSecretBox(t *testing.T) *SecretBox {
	t.Helper()
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	box, err := NewSecretBox()
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	return box
}

func TestSecretBoxRoundTrip(t *testing.T) {
	box := newTestSecretBox(t)

	ciphertext, err := box.Encrypt("sk-super-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(ciphertext, "enc:v1:") {
		t.Fatalf("unexpected format: %s", ciphertext)
	}
	if !box.IsEncrypted(ciphertext) {
		t.Fatal("IsEncrypted returned false for ciphertext")
	}
	plaintext, err := box.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plaintext != "sk-super-secret" {
		t.Fatalf("round trip mismatch: %q", plaintext)
	}

	// Empty values stay empty.
	if value, _ := box.Encrypt(""); value != "" {
		t.Fatalf("empty encrypt = %q", value)
	}
}

func TestSecretBoxLegacyPlaintext(t *testing.T) {
	box := newTestSecretBox(t)
	plaintext, err := box.Decrypt("legacy-plaintext-key")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plaintext != "legacy-plaintext-key" {
		t.Fatalf("legacy passthrough = %q", plaintext)
	}
}

func TestSecretBoxKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".secret-key")
	t.Setenv("SECRET_KEY_FILE", path)
	t.Setenv("SECRET_ENCRYPTION_KEY", "")

	first, err := NewSecretBox()
	if err != nil {
		t.Fatalf("first NewSecretBox: %v", err)
	}
	ciphertext, err := first.Encrypt("value")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// A second instance reads the persisted key and must decrypt the same value.
	second, err := NewSecretBox()
	if err != nil {
		t.Fatalf("second NewSecretBox: %v", err)
	}
	plaintext, err := second.Decrypt(ciphertext)
	if err != nil || plaintext != "value" {
		t.Fatalf("reloaded key failed: %q, %v", plaintext, err)
	}
}

func TestValidateUpstreamURL(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "")

	if _, err := ValidateUpstreamURL("https://api.openai.com/v1"); err != nil {
		t.Fatalf("public URL rejected: %v", err)
	}
	for _, blocked := range []string{
		"http://127.0.0.1:11434/v1",
		"http://localhost:8080",
		"http://10.0.0.5/x",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
	} {
		if _, err := ValidateUpstreamURL(blocked); err == nil {
			t.Fatalf("expected %s to be blocked", blocked)
		}
	}

	// The opt-in flag relaxes the private-host check.
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "yes")
	if _, err := ValidateUpstreamURL("http://127.0.0.1:11434/v1"); err != nil {
		t.Fatalf("private URL rejected despite opt-in: %v", err)
	}
}

func TestIsValidConfigName(t *testing.T) {
	valid := []string{"weather", "api/*", "api/**", "a/b/c"}
	for _, name := range valid {
		if !IsValidConfigName(name) {
			t.Fatalf("expected %q to be valid", name)
		}
	}
	invalid := []string{"", "/api", "api/", "foo*", "***", "a//b"}
	for _, name := range invalid {
		if IsValidConfigName(name) {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}
