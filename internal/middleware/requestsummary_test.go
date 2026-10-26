package middleware

import "testing"

func TestRequestSummaryDecodesTopLevelFields(t *testing.T) {
	body := &parsedBody{raw: []byte(`{
		"model":"m","stream":true,
		"messages":[{"role":"user","content":"hello"}],
		"temperature":0.5,"max_tokens":10,
		"user":"alice","metadata":{"user_id":"u1"},
		"tools":[{"type":"function","function":{"name":"x"}}]
	}`)}
	summary, ok := body.requestSummary()
	if !ok {
		t.Fatal("summary not decoded")
	}
	if summary.Model != "m" || !summary.Stream || !summary.HasMessages {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Temperature == nil || *summary.Temperature != 0.5 {
		t.Fatalf("temperature = %v", summary.Temperature)
	}
	if summary.MaxTokens == nil || *summary.MaxTokens != 10 {
		t.Fatalf("max tokens = %v", summary.MaxTokens)
	}
	if summary.User != "alice" || summary.UserID != "u1" {
		t.Fatalf("identity = %q/%q", summary.User, summary.UserID)
	}
	// The full map must not be decoded just to build the summary.
	if body.json != nil {
		t.Fatal("summary path decoded the full JSON object")
	}
}

func TestRequestSummaryTolerantAndInvalid(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		ok       bool
		model    string
		messages bool
		stream   bool
	}{
		{"empty messages", `{"model":"m","messages":[]}`, true, "m", false, false},
		{"missing stream", `{"model":"m","messages":[{}]}`, true, "m", true, false},
		{"non-string model", `{"model":123,"messages":[{}]}`, true, "", true, false},
		{"non-bool stream", `{"model":"m","stream":"yes","messages":[{}]}`, true, "m", true, false},
		{"bad messages type", `{"model":"m","messages":{}}`, true, "m", false, false},
		{"metadata not object", `{"model":"m","metadata":"x","messages":[{}]}`, true, "m", true, false},
		{"array body", `[]`, false, "", false, false},
		{"null body", `null`, false, "", false, false},
		{"invalid json", `{"model":`, false, "", false, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			summary, ok := (&parsedBody{raw: []byte(test.body)}).requestSummary()
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if summary.Model != test.model || summary.HasMessages != test.messages || summary.Stream != test.stream {
				t.Fatalf("summary = %+v", summary)
			}
		})
	}
}

func TestRequestSummaryStreamValidity(t *testing.T) {
	absent, _ := (&parsedBody{raw: []byte(`{"model":"m","messages":[{}]}`)}).requestSummary()
	if !absent.StreamKnown || absent.Stream {
		t.Fatalf("absent stream = %+v", absent)
	}
	valid, _ := (&parsedBody{raw: []byte(`{"model":"m","stream":true,"messages":[{}]}`)}).requestSummary()
	if !valid.StreamKnown || !valid.Stream {
		t.Fatalf("valid stream = %+v", valid)
	}
	invalid, _ := (&parsedBody{raw: []byte(`{"model":"m","stream":"yes","messages":[{}]}`)}).requestSummary()
	if invalid.StreamKnown {
		t.Fatalf("invalid stream should not be authoritative: %+v", invalid)
	}
}
