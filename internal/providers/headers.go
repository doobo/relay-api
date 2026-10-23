package providers

import (
	"regexp"
	"strings"
)

// SessionHeaderName is the canonical name custom provider headers use to pull
// in the resolved client identity.
const SessionHeaderName = "_ai_session"

var headerPlaceholderPattern = regexp.MustCompile(`\$\{\s*(headers\.)?([\w-]+)\s*\}`)

// renderHeaderValue resolves ${name} and ${headers.name} against the client
// headers, plus ${_ai_session} against the resolved session. Unresolved
// placeholders become empty so a header can be omitted rather than sent with a
// literal template.
func renderHeaderValue(template string, request Request) string {
	// Most provider headers are static; skip the regex machinery entirely.
	if !strings.Contains(template, "${") {
		return sanitizeHeaderValue(template)
	}
	rendered := headerPlaceholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		parts := headerPlaceholderPattern.FindStringSubmatch(match)
		name := strings.ToLower(parts[2])
		if name == SessionHeaderName {
			return request.Session
		}
		return request.ClientHeaders[name]
	})
	return sanitizeHeaderValue(rendered)
}

// sanitizeHeaderValue strips control characters only when present. The common
// case (a clean static header value) is returned without allocating.
func sanitizeHeaderValue(value string) string {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return strings.Map(sanitizeHeaderRune, value)
		}
	}
	return value
}

// sanitizeHeaderRune drops control characters that would make net/http reject
// the header value.
func sanitizeHeaderRune(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

// applyCustomHeaders merges cfg.Headers into headers, rendering placeholders.
// Values that render empty are skipped so optional client headers do not turn
// into empty upstream headers.
func applyCustomHeaders(headers map[string]string, cfg Config, request Request) map[string]string {
	for key, value := range cfg.Headers {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		rendered := renderHeaderValue(value, request)
		if rendered == "" && strings.Contains(value, "${") {
			continue
		}
		headers[name] = rendered
	}
	if request.OnUpstreamHeaders != nil {
		request.OnUpstreamHeaders(headers)
	}
	return headers
}
