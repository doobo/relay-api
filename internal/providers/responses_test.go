package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesProvider(baseURL string) *Responses {
	return &Responses{OpenAI: OpenAI{cfg: Config{
		Type:    "openai-responses",
		BaseURL: baseURL,
	}}}
}

func TestResponsesBodyConversion(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "developer", "content": "sys2"},
			{"role": "user", "content": [
				{"type": "text", "text": "hello"},
				{"type": "image_url", "image_url": {"url": "https://example.com/a.png", "detail": "high"}}
			]},
			{"role": "assistant", "content": "checking", "reasoning_content": "pondering", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "weather", "arguments": "{\"city\":\"Paris\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"temp\":20}"}
		],
		"max_tokens": 100,
		"temperature": 0.2,
		"top_p": 0.9,
		"parallel_tool_calls": false,
		"reasoning_effort": "high",
		"tools": [{"type": "function", "function": {
			"name": "weather", "description": "Get weather", "parameters": {"type": "object"}, "strict": true
		}}],
		"tool_choice": {"type": "function", "function": {"name": "weather"}},
		"response_format": {"type": "json_schema", "json_schema": {
			"name": "answer", "description": "An answer", "schema": {"type": "object"}, "strict": true
		}}
	}`), &raw); err != nil {
		t.Fatalf("decode raw body: %v", err)
	}

	maxTokens := 100
	temperature := 0.2
	provider := responsesProvider("https://example.com/v1")
	body := provider.toResponsesBody(Request{
		Model:       "gpt-6-luna",
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		RawBody:     raw,
	}, true)

	if body["model"] != "gpt-6-luna" || body["stream"] != true {
		t.Fatalf("unexpected model/stream: %+v", body)
	}
	if body["instructions"] != "sys\nsys2" {
		t.Fatalf("instructions = %#v", body["instructions"])
	}
	if body["max_output_tokens"] != 100 || body["temperature"] != 0.2 || body["top_p"] != 0.9 {
		t.Fatalf("generation options not mapped: %+v", body)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}

	input, _ := body["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input length = %d: %#v", len(input), input)
	}
	user, _ := input[0].(map[string]any)
	content, _ := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("user content = %#v", content)
	}
	if text, _ := content[0].(map[string]any); text["type"] != "input_text" || text["text"] != "hello" {
		t.Fatalf("text part = %#v", content[0])
	}
	if image, _ := content[1].(map[string]any); image["type"] != "input_image" || image["image_url"] != "https://example.com/a.png" || image["detail"] != "high" {
		t.Fatalf("image part = %#v", content[1])
	}
	reasoningItem, _ := input[1].(map[string]any)
	reasoningSummary, _ := reasoningItem["summary"].([]any)
	reasoningPart, _ := reasoningSummary[0].(map[string]any)
	if reasoningItem["type"] != "reasoning" || reasoningPart["type"] != "summary_text" || reasoningPart["text"] != "pondering" {
		t.Fatalf("reasoning item = %#v", reasoningItem)
	}
	assistant, _ := input[2].(map[string]any)
	assistantContent, _ := assistant["content"].([]any)
	assistantText, _ := assistantContent[0].(map[string]any)
	if assistant["role"] != "assistant" || assistantText["type"] != "output_text" || assistantText["text"] != "checking" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	call, _ := input[3].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "weather" || call["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("function call = %#v", call)
	}
	toolResult, _ := input[4].(map[string]any)
	if toolResult["type"] != "function_call_output" || toolResult["call_id"] != "call_1" || toolResult["output"] != `{"temp":20}` {
		t.Fatalf("function output = %#v", toolResult)
	}

	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "weather" || tool["description"] != "Get weather" || tool["strict"] != true {
		t.Fatalf("tool = %#v", tool)
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "weather" {
		t.Fatalf("tool choice = %#v", body["tool_choice"])
	}
	text, _ := body["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" || format["strict"] != true {
		t.Fatalf("text format = %#v", body["text"])
	}
}

func TestResponsesChatParsesOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "resp_1", "object": "response", "created_at": 123, "model": "gpt-6-luna",
			"status": "completed",
			"output": [
				{"type": "reasoning", "summary": [{"type": "summary_text", "text": "thinking"}]},
				{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hello"}]},
				{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "weather", "arguments": "{\"city\":\"Paris\"}"}
			],
			"usage": {"input_tokens": 5, "output_tokens": 7, "total_tokens": 12}
		}`)
	}))
	defer server.Close()

	result, err := responsesProvider(server.URL+"/v1").Chat(context.Background(), Request{Model: "gpt-6-luna", RawBody: map[string]any{}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.ID != "resp_1" || result.Created != 123 || result.Model != "gpt-6-luna" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Message.Content == nil || *result.Message.Content != "hello" {
		t.Fatalf("content = %#v", result.Message.Content)
	}
	if result.Message.ReasoningContent == nil || *result.Message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning = %#v", result.Message.ReasoningContent)
	}
	if len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].ID != "call_1" || result.Message.ToolCalls[0].Function.Name != "weather" {
		t.Fatalf("tool calls = %+v", result.Message.ToolCalls)
	}
	if result.FinishReason == nil || *result.FinishReason != "tool_calls" {
		t.Fatalf("finish = %#v", result.FinishReason)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 5 || result.Usage.CompletionTokens != 7 || result.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestResponsesStreamConversion(t *testing.T) {
	events := []string{
		`event: response.created
data: {"type":"response.created","response":{"id":"resp_s","model":"gpt-6-luna","created_at":1}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1"}}`,
		`event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":0,"delta":"think"}`,
		`event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","summary_index":0,"text":"think"}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hel"}`,
		`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":""}}`,
		`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":\"Paris\"}"}`,
		`event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"city\":\"Paris\"}"}`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_s","model":"gpt-6-luna","status":"completed","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"}]}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, event+"\n\n")
		}
	}))
	defer server.Close()

	response, err := responsesProvider(server.URL+"/v1").ChatStream(context.Background(), Request{Model: "gpt-6-luna", RawBody: map[string]any{}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	stream := string(body)

	for _, want := range []string{
		`"reasoning_content":"think"`,
		`"content":"hel"`,
		`"content":"lo"`,
		`"id":"call_1"`,
		`"name":"weather"`,
		`"arguments":"{\"city\":\"Paris\"}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":5`,
		`"completion_tokens":7`,
		`"total_tokens":12`,
		`data: [DONE]`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %s:\n%s", want, stream)
		}
	}
	if strings.Count(stream, `"reasoning_content":"think"`) != 1 {
		t.Fatalf("reasoning was duplicated:\n%s", stream)
	}
	if strings.Count(stream, `"arguments":"{\"city\":\"Paris\"}"`) != 1 {
		t.Fatalf("tool arguments were duplicated:\n%s", stream)
	}
}
