package util

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// Login payload encryption (port of utils/login-crypto.ts).
//
// The admin UI never sends the password in the clear. GET /admin/auth/challenge
// hands out two public keys and a one-time nonce; the browser picks a scheme:
//
//  1. WebCrypto (preferred): a fresh AES-GCM key encrypts
//     {username, password, nonce} and is wrapped with the RSA-OAEP public key.
//  2. TweetNaCl box (fallback, no crypto.subtle): X25519 + XSalsa20-Poly1305.
//
// Scope: these keep the password out of the request body for a passive
// observer. They are NOT a substitute for TLS. Nonces are single-use.

const (
	nonceTTL    = 2 * time.Minute
	nonceLength = 24
)

// Credentials are the decrypted username/password pair.
type Credentials struct {
	Username string
	Password string
}

// JWK is the bare RSA public key the browser imports with importKey("jwk", …).
type JWK struct {
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// BoxKey is the TweetNaCl fallback public key.
type BoxKey struct {
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"publicKey"`
}

// Challenge is the GET /admin/auth/challenge response.
type Challenge struct {
	Algorithm string `json:"algorithm"`
	PublicKey JWK    `json:"publicKey"`
	Nonce     string `json:"nonce"`
	Box       BoxKey `json:"box"`
}

// ChallengeStore owns the process-lifetime RSA and X25519 key pairs plus the
// table of outstanding one-time nonces.
type ChallengeStore struct {
	mu     sync.Mutex
	nonces map[string]int64 // nonce -> expiry (epoch ms)

	rsaOnce sync.Once
	rsaKey  *rsa.PrivateKey
	rsaErr  error

	boxOnce sync.Once
	boxPub  *[32]byte
	boxPriv *[32]byte
	boxErr  error
}

// NewChallengeStore creates an empty store; key pairs are generated lazily on
// first use, exactly like the reference.
func NewChallengeStore() *ChallengeStore {
	return &ChallengeStore{nonces: make(map[string]int64)}
}

// Issue returns the public keys and a fresh single-use nonce.
func (s *ChallengeStore) Issue() (Challenge, error) {
	key, err := s.rsaKeyPair()
	if err != nil {
		return Challenge{}, err
	}

	now := time.Now().UnixMilli()
	s.mu.Lock()
	for nonce, expiresAt := range s.nonces {
		if expiresAt <= now {
			delete(s.nonces, nonce)
		}
	}
	nonce := RandomToken(nonceLength)
	s.nonces[nonce] = now + nonceTTL.Milliseconds()
	s.mu.Unlock()

	pub, err := s.boxPublicKey()
	if err != nil {
		return Challenge{}, err
	}

	return Challenge{
		Algorithm: "RSA-OAEP-256+A256GCM",
		PublicKey: rsaPublicJWK(&key.PublicKey),
		Nonce:     nonce,
		Box: BoxKey{
			Algorithm: "x25519-xsalsa20-poly1305",
			PublicKey: base64.StdEncoding.EncodeToString(pub[:]),
		},
	}, nil
}

// DecryptWebCrypto decrypts the `{encrypted:{key,iv,data}}` payload: RSA-OAEP
// unwraps the AES key, AES-GCM decrypts the JSON credentials.
func (s *ChallengeStore) DecryptWebCrypto(keyB64, ivB64, dataB64 string) (Credentials, error) {
	key, err := s.rsaKeyPair()
	if err != nil {
		return Credentials{}, err
	}
	wrapped, err := decodeBase64(keyB64)
	if err != nil {
		return Credentials{}, err
	}
	rawKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, wrapped, nil)
	if err != nil {
		return Credentials{}, err
	}
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		return Credentials{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Credentials{}, err
	}
	iv, err := decodeBase64(ivB64)
	if err != nil {
		return Credentials{}, err
	}
	if len(iv) != gcm.NonceSize() {
		return Credentials{}, errors.New("invalid AES-GCM iv length")
	}
	ciphertext, err := decodeBase64(dataB64)
	if err != nil {
		return Credentials{}, err
	}
	plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return Credentials{}, err
	}
	return s.parseCredentials(plaintext)
}

// DecryptBox decrypts the `{box:{publicKey,nonce,data}}` TweetNaCl payload.
func (s *ChallengeStore) DecryptBox(publicKeyB64, nonceB64, dataB64 string) (Credentials, error) {
	_, priv, err := s.boxKeyPair()
	if err != nil {
		return Credentials{}, err
	}

	clientPubBytes, err := decodeBase64(publicKeyB64)
	if err != nil {
		return Credentials{}, err
	}
	if len(clientPubBytes) != 32 {
		return Credentials{}, errors.New("invalid box public key length")
	}
	var clientPub [32]byte
	copy(clientPub[:], clientPubBytes)

	nonceBytes, err := decodeBase64(nonceB64)
	if err != nil {
		return Credentials{}, err
	}
	if len(nonceBytes) != 24 {
		return Credentials{}, errors.New("invalid box nonce length")
	}
	var nonce [24]byte
	copy(nonce[:], nonceBytes)

	ciphertext, err := decodeBase64(dataB64)
	if err != nil {
		return Credentials{}, err
	}
	opened, ok := box.Open(nil, ciphertext, &nonce, &clientPub, priv)
	if !ok {
		return Credentials{}, errors.New("box decryption failed")
	}
	return s.parseCredentials(opened)
}

func (s *ChallengeStore) parseCredentials(raw []byte) (Credentials, error) {
	var parsed struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nonce    string `json:"nonce"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Credentials{}, errors.New("malformed login payload")
	}
	if !s.consumeNonce(parsed.Nonce) {
		return Credentials{}, errors.New("unknown or expired nonce")
	}
	return Credentials{Username: parsed.Username, Password: parsed.Password}, nil
}

// consumeNonce returns true exactly once per nonce, and only before it expires.
func (s *ChallengeStore) consumeNonce(nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	delete(s.nonces, nonce)
	return expiresAt > time.Now().UnixMilli()
}

func (s *ChallengeStore) rsaKeyPair() (*rsa.PrivateKey, error) {
	s.rsaOnce.Do(func() {
		s.rsaKey, s.rsaErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	return s.rsaKey, s.rsaErr
}

func (s *ChallengeStore) boxKeyPair() (*[32]byte, *[32]byte, error) {
	s.boxOnce.Do(func() {
		pub, priv, err := box.GenerateKey(rand.Reader)
		s.boxPub, s.boxPriv, s.boxErr = pub, priv, err
	})
	return s.boxPub, s.boxPriv, s.boxErr
}

func (s *ChallengeStore) boxPublicKey() (*[32]byte, error) {
	pub, _, err := s.boxKeyPair()
	return pub, err
}

func rsaPublicJWK(pub *rsa.PublicKey) JWK {
	return JWK{
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// decodeBase64 accepts standard base64 with or without padding, which covers
// both btoa() output and the raw form some clients emit.
func decodeBase64(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}
