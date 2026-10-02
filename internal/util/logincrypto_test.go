package util

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// TestDecryptWebCrypto exercises the preferred browser path: a fresh AES-GCM
// key encrypted by the server's RSA-OAEP public key, wrapping JSON credentials.
func TestDecryptWebCrypto(t *testing.T) {
	store := NewChallengeStore()
	challenge, err := store.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand iv: %v", err)
	}
	plaintext, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
		"nonce":    challenge.Nonce,
	})
	data := gcm.Seal(nil, iv, plaintext, nil)

	// Wrap the AES key with the public half of the server's JWK.
	nBytes, err := base64.RawURLEncoding.DecodeString(challenge.PublicKey.N)
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(challenge.PublicKey.E)
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(new(big.Int).SetBytes(eBytes).Int64())}
	if pub.E != 65537 {
		t.Fatalf("unexpected exponent: %d", pub.E)
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		t.Fatalf("rsa encrypt: %v", err)
	}

	creds, err := store.DecryptWebCrypto(encode(wrapped), encode(iv), encode(data))
	if err != nil {
		t.Fatalf("DecryptWebCrypto: %v", err)
	}
	if creds.Username != "admin" || creds.Password != "admin123" {
		t.Fatalf("unexpected credentials: %+v", creds)
	}

	// The nonce is single-use: replaying the same payload must fail.
	if _, err := store.DecryptWebCrypto(encode(wrapped), encode(iv), encode(data)); err == nil {
		t.Fatal("expected replay to fail, got nil error")
	}
}

// TestDecryptBox exercises the WebCrypto-free fallback (X25519 + XSalsa20).
func TestDecryptBox(t *testing.T) {
	store := NewChallengeStore()
	challenge, err := store.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	serverPubBytes, err := base64.StdEncoding.DecodeString(challenge.Box.PublicKey)
	if err != nil {
		t.Fatalf("decode box key: %v", err)
	}
	var serverPub [32]byte
	copy(serverPub[:], serverPubBytes)

	clientPub, clientPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("rand nonce: %v", err)
	}
	plaintext, _ := json.Marshal(map[string]string{
		"username": "admin",
		"password": "admin123",
		"nonce":    challenge.Nonce,
	})
	sealed := box.Seal(nil, plaintext, &nonce, &serverPub, clientPriv)

	creds, err := store.DecryptBox(encode(clientPub[:]), encode(nonce[:]), encode(sealed))
	if err != nil {
		t.Fatalf("DecryptBox: %v", err)
	}
	if creds.Username != "admin" || creds.Password != "admin123" {
		t.Fatalf("unexpected credentials: %+v", creds)
	}

	// Unknown nonce (replay) must fail too.
	if _, err := store.DecryptBox(encode(clientPub[:]), encode(nonce[:]), encode(sealed)); err == nil {
		t.Fatal("expected replay to fail, got nil error")
	}
}
