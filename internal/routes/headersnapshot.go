package routes

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// maxHeaderSnapshotBytes caps a stored header snapshot so an unusual client
// cannot turn the usage table into a blob store.
const maxHeaderSnapshotBytes = 16 * 1024

// redactedHeaders are stored as [redacted]: they are credentials or browser
// state, not useful request metadata.
var redactedHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
	"x-goog-api-key":      true,
	"x-openai-api-key":    true,
	"x-auth-token":        true,
	"x-access-token":      true,
}

// snapshotHTTPHeaders serializes client headers for the usage detail view.
func snapshotHTTPHeaders(header http.Header) *string {
	if len(header) == 0 {
		return nil
	}
	headers := make(map[string]string, len(header))
	for key, values := range header {
		if len(values) == 0 {
			continue
		}
		headers[strings.ToLower(key)] = strings.Join(values, ", ")
	}
	return encodeHeaderSnapshot(headers)
}

// snapshotHeaderMap serializes an already-normalized header map (the effective
// headers sent upstream).
func snapshotHeaderMap(headers map[string]string) *string {
	if len(headers) == 0 {
		return nil
	}
	return encodeHeaderSnapshot(headers)
}

func encodeHeaderSnapshot(headers map[string]string) *string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	safe := make(map[string]string, len(headers))
	budget := maxHeaderSnapshotBytes
	for _, key := range keys {
		name := strings.ToLower(strings.TrimSpace(key))
		if name == "" {
			continue
		}
		value := headers[key]
		if redactedHeaders[name] {
			value = "[redacted]"
		}
		if len(value) > 1024 {
			value = value[:1024]
		}
		cost := len(name) + len(value) + 6 // quotes, colon and comma
		if cost > budget {
			continue
		}
		budget -= cost
		safe[name] = value
	}
	if len(safe) == 0 {
		return nil
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return nil
	}
	serialized := string(encoded)
	return &serialized
}
