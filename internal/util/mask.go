package util

// MaskSecret renders a secret for display: first 4 and last 4 characters, or
// "****" for short values. Port of utils/mask.ts.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}
