package routes

import (
	"errors"
	"net/url"

	"relay-api/internal/db"
	"relay-api/internal/util"
)

// configUpstreamURL returns a private copy of the config's upstream URL.
//
// The parse and the scheme/hostname checks were already done when the config
// cache was built, so a warm request skips url.Parse. The copy is mandatory
// because callers append the wildcard tail and the client's query string.
//
// The SSRF policy stays here on every request: ALLOW_PRIVATE_UPSTREAMS is a
// process-level switch whose error text tells operators to restart, but tests
// (and a future settings reload) flip it between requests, so caching the
// decision would change behaviour.
func configUpstreamURL(config *db.APIConfigRow) (*url.URL, error) {
	parsed := config.ParsedURL
	if parsed == nil {
		// A config that failed to parse when the cache was built keeps its
		// cached error; rows loaded outside the cache (admin reads, tests that
		// never warmed it) are parsed here.
		if config.ParsedURLErr != nil {
			return nil, config.ParsedURLErr
		}
		var err error
		parsed, err = util.ParseUpstreamURL(config.URL)
		if err != nil {
			return nil, err
		}
	}
	if err := util.CheckUpstreamHost(parsed); err != nil {
		return nil, err
	}
	clone := *parsed
	return &clone, nil
}

// configAPIKey returns the plaintext upstream key for a config.
//
// The cache decrypts it once when the config set loads; nil ParsedKey means the
// row was not compiled (admin reads, or a process that never wired a secret
// box), so the call falls back to decrypting on demand and reports the same
// error the per-request path always did.
func configAPIKey(secrets *util.SecretBox, config *db.APIConfigRow) (string, error) {
	if config.APIKey == nil || *config.APIKey == "" {
		return "", nil
	}
	if config.ParsedKey != nil {
		return *config.ParsedKey, nil
	}
	if config.ParsedKeyErr != nil {
		return "", config.ParsedKeyErr
	}
	if secrets == nil {
		return "", errors.New("no secret box configured")
	}
	return secrets.Decrypt(*config.APIKey)
}
