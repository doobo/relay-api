package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const anthropicToolRequest = `{"model":"claude","max_tokens":100,"parallel_tool_calls":false,
"tool_choice":{"type":"function","function":{"name":"get_weather"}},
"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
"messages":[
{"role":"system","content":"be brief"},
{"role":"user","content":"weather in Paris?"},
{"role":"assistant","content":null,"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
{"role":"tool","tool_call_id":"toolu_1","content":"18C"}]}`

// assertJSONEqual compares a value against expected JSON, ignoring key order.
func assertJSONEqual(t *testing.T, name string, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatalf("%s: decode got: %v", name, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: decode want: %v", name, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s:\n got  %s\n want %s", name, encoded, want)
	}
}

func anthropicTestProvider(baseURL string) *Anthropic {
	return NewAnthropic(Config{Name: "claude", Type: "anthropic", BaseURL: baseURL, RequestTimeoutMs: 5000, HeaderTimeoutMs: 5000})
}

func TestAnthropicToolsNonStream(t *testing.T) {
	var sent map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_2","model":"claude-x","stop_reason":"tool_use",`+
			`"content":[{"type":"text","text":"Checking"},{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"Lyon"}}],`+
			`"usage":{"input_tokens":9,"output_tokens":4}}`)
	}))
	defer upstream.Close()

	var raw map[string]any
	if err := json.Unmarshal([]byte(anthropicToolRequest), &raw); err != nil {
		t.Fatalf("request: %v", err)
	}
	result, err := anthropicTestProvider(upstream.URL).Chat(context.Background(), Request{Model: "claude-x", RawBody: raw})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if sent["system"] != "be brief" {
		t.Fatalf("system = %v", sent["system"])
	}
	assertJSONEqual(t, "tools", sent["tools"], `[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`)
	assertJSONEqual(t, "tool_choice", sent["tool_choice"], `{"type":"tool","name":"get_weather","disable_parallel_tool_use":true}`)
	assertJSONEqual(t, "messages", sent["messages"], `[
		{"role":"user","content":[{"type":"text","text":"weather in Paris?"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"18C"}]}]`)

	if len(result.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", result.Message.ToolCalls)
	}
	call := result.Message.ToolCalls[0]
	if call.ID != "toolu_2" || call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Lyon"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if result.Message.Content == nil || *result.Message.Content != "Checking" {
		t.Fatalf("content = %v", result.Message.Content)
	}
	if result.FinishReason == nil || *result.FinishReason != "tool_calls" {
		t.Fatalf("finish = %v", result.FinishReason)
	}
}

func TestAnthropicToolsStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer upstream.Close()

	var raw map[string]any
	if err := json.Unmarshal([]byte(anthropicToolRequest), &raw); err != nil {
		t.Fatalf("request: %v", err)
	}
	response, err := anthropicTestProvider(upstream.URL).ChatStream(context.Background(), Request{Model: "claude-x", Stream: true, RawBody: raw})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}

	var (
		callID, callName, arguments string
		finish                      string
	)
	for _, event := range strings.Split(string(body), "\n\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(event), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("chunk %q: %v", data, err)
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				if call.ID != "" {
					callID = call.ID
					callName = call.Function.Name
				}
				arguments += call.Function.Arguments
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if callID != "toolu_9" || callName != "get_weather" || arguments != `{"city":"Paris"}` {
		t.Fatalf("streamed tool call = %q %q %q\n%s", callID, callName, arguments, body)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}
