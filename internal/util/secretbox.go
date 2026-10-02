package util

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// SecretBox encrypts sensitive columns (provider api_key, api_config api_key)
// at rest with AES-256-GCM. The ciphertext format is
// `enc:v1:<iv_b64>:<tag_b64>:<ct_b64>`, identical to the reference
// utils/secretbox.ts, so a database written by the reference stays readable and
// vice versa. Values without the prefix are legacy plaintext and pass through.
const secretPrefix = "enc:v1:"

// SecretBox holds the 32-byte encryption key.
type SecretBox struct {
	key []byte
}

// NewSecretBox loads the encryption key from SECRET_ENCRYPTION_KEY, or from the
// key file (SECRET_KEY_FILE, default data/.secret-key), generating and
// persisting a fresh one on first start.
func NewSecretBox() (*SecretBox, error) {
	key, err := loadSecretKey()
	if err != nil {
		return nil, err
	}
	return &SecretBox{key: key}, nil
}

// Encrypt returns the storable ciphertext. Empty input stays empty.
func (s *SecretBox) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	gcm, err := s.gcm()
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	// The reference stores tag and ciphertext separately; split them so the
	// format matches byte for byte.
	tag := sealed[len(sealed)-gcm.Overhead():]
	ciphertext := sealed[:len(sealed)-gcm.Overhead()]
	return secretPrefix +
		base64.StdEncoding.EncodeToString(iv) + ":" +
		base64.StdEncoding.EncodeToString(tag) + ":" +
		base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt returns the plaintext; unprefixed values are legacy plaintext and are
// returned unchanged.
func (s *SecretBox) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, secretPrefix) {
		return stored, nil
	}
	parts := strings.Split(strings.TrimPrefix(stored, secretPrefix), ":")
	if len(parts) != 3 {
		return "", errors.New("malformed encrypted secret")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	tag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	gcm, err := s.gcm()
	if err != nil {
		return "", err
	}
	if len(iv) != gcm.NonceSize() {
		return "", errors.New("malformed encrypted secret")
	}
	sealed := append(append([]byte{}, ciphertext...), tag...)
	plaintext, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// IsEncrypted reports whether the stored value is in encrypted format.
func (s *SecretBox) IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, secretPrefix)
}

func (s *SecretBox) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func loadSecretKey() ([]byte, error) {
	if envKey := os.Getenv("SECRET_ENCRYPTION_KEY"); envKey != "" {
		raw, err := base64.StdEncoding.DecodeString(envKey)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("SECRET_ENCRYPTION_KEY must be 32 bytes base64-encoded. Generate one with: openssl rand -base64 32")
		}
		return raw, nil
	}

	keyPath := os.Getenv("SECRET_KEY_FILE")
	if keyPath == "" {
		keyPath = filepath.Join("data", ".secret-key")
	}
	if contents, err := os.ReadFile(keyPath); err == nil {
		raw, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(contents)))
		if decodeErr != nil || len(raw) != 32 {
			return nil, fmt.Errorf("secret key file %s is corrupted (expected 32-byte base64)", keyPath)
		}
		return raw, nil
	}

	// First start: generate a random key and persist it next to the database.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	slog.Info("generated new secret encryption key", "path", keyPath)
	return key, nil
}
