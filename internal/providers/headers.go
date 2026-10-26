package providers

import (
	"regexp"
	"strings"
)

// SessionHeaderName is the canonical name custom provider headers use to pull
// in the resolved client identity.
const SessionHeaderName = "_ai_session"

var headerPlaceholderPattern = regexp.MustCompile(`\$\{\s*(headers\.)?([\w-]+)\s*\}`)

type headerSegment struct {
	literal  string
	lookup   string
	isLookup bool
}

// HeaderTemplate is one pre-parsed custom provider header. Values with
// ${...} placeholders are split into literal/lookup segments once when the
// provider config is cached, so the relay no longer runs a regex per header per
// request.
type HeaderTemplate struct {
	name     string
	value    string
	dynamic  bool
	segments []headerSegment
}

// CompileHeaders parses a provider's custom header map into templates.
func CompileHeaders(headers map[string]string) []HeaderTemplate {
	if len(headers) == 0 {
		return nil
	}
	templates := make([]HeaderTemplate, 0, len(headers))
	for key, value := range headers {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		template := HeaderTemplate{name: name}
		matches := headerPlaceholderPattern.FindAllStringSubmatchIndex(value, -1)
		if len(matches) == 0 {
			template.value = sanitizeHeaderValue(value)
			templates = append(templates, template)
			continue
		}
		template.dynamic = true
		last := 0
		for _, match := range matches {
			if match[0] > last {
				template.segments = append(template.segments, headerSegment{literal: value[last:match[0]]})
			}
			if match[4] >= 0 {
				template.segments = append(template.segments, headerSegment{
					lookup:   strings.ToLower(value[match[4]:match[5]]),
					isLookup: true,
				})
			}
			last = match[1]
		}
		if last < len(value) {
			template.segments = append(template.segments, headerSegment{literal: value[last:]})
		}
		templates = append(templates, template)
	}
	return templates
}

// render resolves the template. set is false when a dynamic value renders
// empty, so optional client headers do not turn into empty upstream headers.
func (t HeaderTemplate) render(request Request) (value string, set bool) {
	if !t.dynamic {
		return t.value, true
	}
	var builder strings.Builder
	for _, segment := range t.segments {
		if !segment.isLookup {
			builder.WriteString(segment.literal)
			continue
		}
		if segment.lookup == SessionHeaderName {
			builder.WriteString(request.Session)
		} else {
			builder.WriteString(request.ClientHeaders[segment.lookup])
		}
	}
	value = sanitizeHeaderValue(builder.String())
	if value == "" {
		return "", false
	}
	return value, true
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
// Compiled templates are used when present; directly constructed configs (and
// tests) fall back to one compile per call.
func applyCustomHeaders(headers map[string]string, cfg Config, request Request) map[string]string {
	templates := cfg.HeaderTemplates
	if templates == nil && len(cfg.Headers) > 0 {
		templates = CompileHeaders(cfg.Headers)
	}
	for _, template := range templates {
		if value, set := template.render(request); set {
			headers[template.name] = value
		}
	}
	if request.OnUpstreamHeaders != nil {
		request.OnUpstreamHeaders(headers)
	}
	return headers
}
