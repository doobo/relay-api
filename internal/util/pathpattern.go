package util

import "strings"

// MatchConfigPattern matches `name` against a request path relative to a
// forwarding mount. It returns the path tail from the first wildcard onward
// ("a/b" for name `api/**` and path `api/a/b`), or ok=false when the pattern
// does not apply. A pattern without wildcards matches only the identical path
// (empty tail).
func MatchConfigPattern(name, path string) (string, bool) {
	patternParts := splitSegments(name)
	pathParts := splitSegments(path)
	prefix := literalPrefixCount(patternParts)
	if len(pathParts) < prefix {
		return "", false
	}
	for index := 0; index < prefix; index++ {
		if patternParts[index] != pathParts[index] {
			return "", false
		}
	}
	if !matchTail(patternParts[prefix:], pathParts[prefix:]) {
		return "", false
	}
	return strings.Join(pathParts[prefix:], "/"), true
}

func splitSegments(value string) []string {
	parts := strings.Split(value, "/")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// literalPrefixCount is where the first wildcard sits.
func literalPrefixCount(parts []string) int {
	for index, part := range parts {
		if part == "*" || part == "**" {
			return index
		}
	}
	return len(parts)
}

func matchTail(patternParts, pathParts []string) bool {
	if len(patternParts) == 0 {
		return len(pathParts) == 0
	}
	head := patternParts[0]
	tail := patternParts[1:]
	switch head {
	case "**":
		// One or more segments: `api/**` covers /open/api/a and /open/api/a/b
		// but not /open/api.
		for take := 1; take <= len(pathParts); take++ {
			if matchTail(tail, pathParts[take:]) {
				return true
			}
		}
		return false
	case "*":
		return len(pathParts) > 0 && matchTail(tail, pathParts[1:])
	default:
		return len(pathParts) > 0 && pathParts[0] == head && matchTail(tail, pathParts[1:])
	}
}

// ComparePatternSpecificity orders two matching patterns: more literal prefix
// first, then more literal segments, then the narrower wildcard (`api/*` beats
// `api/**`), then the deeper pattern. Positive means a is more specific than b.
func ComparePatternSpecificity(a, b string) int {
	aParts := splitSegments(a)
	bParts := splitSegments(b)
	if prefix := literalPrefixCount(aParts) - literalPrefixCount(bParts); prefix != 0 {
		return prefix
	}
	if literals := literalCount(aParts) - literalCount(bParts); literals != 0 {
		return literals
	}
	if globstars := globstarCount(bParts) - globstarCount(aParts); globstars != 0 {
		return globstars
	}
	return len(aParts) - len(bParts)
}

func literalCount(parts []string) int {
	count := 0
	for _, part := range parts {
		if part != "*" && part != "**" {
			count++
		}
	}
	return count
}

func globstarCount(parts []string) int {
	count := 0
	for _, part := range parts {
		if part == "**" {
			count++
		}
	}
	return count
}

// IsWildcardName reports whether a config name contains a wildcard.
func IsWildcardName(name string) bool {
	return strings.Contains(name, "*")
}

// IsValidConfigName validates an API-config name pattern: wildcards (`*`,
// `**`) must fill a whole path segment, and names may not be empty or start/end
// with a slash. Port of isValidConfigName.
func IsValidConfigName(name string) bool {
	if strings.TrimSpace(name) == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" {
			return false
		}
		if strings.Contains(segment, "*") && segment != "*" && segment != "**" {
			return false
		}
	}
	return true
}
