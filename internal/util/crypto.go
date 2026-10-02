package util

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost matches the reference (Bun.password cost 10).
const bcryptCost = 10

// SHA256Hex returns the lowercase hex SHA-256 of input. API keys and session
// tokens are stored only as this hash.
func SHA256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// HashPassword returns a bcrypt hash ($2a$...; Bun emits $2b$, both verify).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword reports whether password matches the stored bcrypt hash.
// A malformed hash is a non-match, never an error (matching verifyPassword).
func VerifyPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
