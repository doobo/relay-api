package providers

import "testing"

func TestCustomProviderHeaders(t *testing.T) {
	apiKey := "secret"
	provider := &OpenAI{cfg: Config{
		APIKey: &apiKey,
		Headers: map[string]string{
			"x-user-id":  "${x-user-id}",
			"x-session":  "${_ai_session}",
			"x-uuid":     "${headers.uuid}",
			"x-static":   "fixed",
			"x-missing":  "${nope}",
			"x-override": "custom",
		},
	}}
	request := Request{
		ClientHeaders: map[string]string{
			"x-user-id": "alice",
			"uuid":      "client-uuid",
		},
		Session: "session-123",
	}

	headers := provider.headers(request)

	want := map[string]string{
		"content-type":  "application/json",
		"authorization": "Bearer secret",
		"x-user-id":     "alice",
		"x-session":     "session-123",
		"x-uuid":        "client-uuid",
		"x-static":      "fixed",
		"x-override":    "custom",
	}
	for key, value := range want {
		if headers[key] != value {
			t.Errorf("%s = %q, want %q", key, headers[key], value)
		}
	}
	if _, ok := headers["x-missing"]; ok {
		t.Errorf("unresolved header should be omitted, got %q", headers["x-missing"])
	}
}

func TestCustomHeadersCanOverrideAuth(t *testing.T) {
	provider := &OpenAI{cfg: Config{
		APIKey:  ptr("stored"),
		Headers: map[string]string{"authorization": "Token ${token}"},
	}}
	headers := provider.headers(Request{ClientHeaders: map[string]string{"token": "abc"}})
	if headers["authorization"] != "Token abc" {
		t.Fatalf("authorization = %q", headers["authorization"])
	}
}

func ptr(value string) *string { return &value }
