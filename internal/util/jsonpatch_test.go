package util

import (
	"encoding/json"
	"testing"
)

func TestRewriteTopLevelJSON(t *testing.T) {
	raw := []byte(`{
  "model" : "alias",
  "messages": [{"role": "user", "content": "hi"}],
  "stream": true,
  "custom": {"keep": [1, 2, 3]}
}`)
	model, _ := json.Marshal("upstream-model")
	rewritten, err := RewriteTopLevelJSON(raw, map[string]json.RawMessage{
		"model":  model,
		"stream": json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	want := `{
  "model" : "upstream-model",
  "messages": [{"role": "user", "content": "hi"}],
  "stream": false,
  "custom": {"keep": [1, 2, 3]}
}`
	if string(rewritten) != want {
		t.Fatalf("rewritten = %q, want %q", rewritten, want)
	}
}

func TestRewriteTopLevelJSONInsertsMissingField(t *testing.T) {
	model, _ := json.Marshal("upstream")
	rewritten, err := RewriteTopLevelJSON([]byte(`{"model":"alias"}`), map[string]json.RawMessage{
		"model":  model,
		"stream": json.RawMessage("false"),
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, want := string(rewritten), `{"stream":false,"model":"upstream"}`; got != want {
		t.Fatalf("rewritten = %q, want %q", got, want)
	}

	rewritten, err = RewriteTopLevelJSON([]byte(`{}`), map[string]json.RawMessage{
		"model": model,
	})
	if err != nil {
		t.Fatalf("rewrite empty: %v", err)
	}
	if got, want := string(rewritten), `{"model":"upstream"}`; got != want {
		t.Fatalf("empty rewritten = %q, want %q", got, want)
	}
}
