package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"relay-api/internal/util"
)

// Responses adapts OpenAI chat-completions requests onto the OpenAI Responses
// wire format (POST /responses). The Responses API is the only protocol offered
// by some upstreams (for example OpenCode Go's GPT and Muse models), so the
// gateway translates both directions and keeps clients on Chat Completions.
type Responses struct {
	OpenAI
}

func (p *Responses) endpoint(path string) string {
	return strings.TrimRight(p.cfg.BaseURL, "/") + path
}

func (p *Responses) headers(request Request) map[string]string {
	return p.OpenAI.headers(request)
}

// toResponsesBody converts an OpenAI chat request into a Responses request.
func (p *Responses) toResponsesBody(request Request, stream bool) map[string]any {
	body := map[string]any{
		"model":  request.Model,
		"stream": stream,
	}

	input, instructions := convertResponsesInput(request.RawBody["messages"])
	body["input"] = input
	if instructions != "" {
		body["instructions"] = instructions
	}

	maxTokens := request.MaxTokens
	if maxTokens == nil {
		if value := intFromAny(request.RawBody["max_completion_tokens"]); value > 0 {
			maxTokens = &value
		}
	}
	if maxTokens != nil {
		body["max_output_tokens"] = *maxTokens
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}

	for _, key := range []string{
		"top_p", "top_logprobs", "store", "metadata", "service_tier",
		"truncation", "background", "parallel_tool_calls", "prompt_cache_key",
		"safety_identifier", "max_tool_calls", "include",
	} {
		if value, ok := request.RawBody[key]; ok {
			body[key] = value
		}
	}

	if tools := convertResponsesTools(request.RawBody["tools"]); len(tools) > 0 {
		body["tools"] = tools
	} else if tools := convertLegacyFunctions(request.RawBody["functions"]); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := convertResponsesToolChoice(request.RawBody["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	} else if choice := convertLegacyFunctionCall(request.RawBody["function_call"]); choice != nil {
		body["tool_choice"] = choice
	}

	if text := convertResponsesText(request.RawBody); text != nil {
		body["text"] = text
	}
	if reasoning := convertResponsesReasoning(request.RawBody); reasoning != nil {
		body["reasoning"] = reasoning
	}
	return body
}

// convertResponsesInput maps chat messages onto Responses input items and folds
// system/developer messages into instructions.
func convertResponsesInput(raw any) ([]any, string) {
	messages, _ := raw.([]any)
	input := make([]any, 0, len(messages))
	var system []string

	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if text := extractContentText(message["content"]); text != "" {
				system = append(system, text)
			}
		case "user":
			if content := convertResponsesUserContent(message["content"]); len(content) > 0 {
				input = append(input, map[string]any{
					"type": "message", "role": "user", "content": content,
				})
			}
		case "assistant":
			if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
				input = append(input, map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
				})
			}
			if text := extractContentText(message["content"]); text != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				})
			}
			for _, call := range toolCallsFromMessage(message) {
				if call.ID == "" || call.Function.Name == "" {
					continue
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				})
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				continue
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  extractContentText(message["content"]),
			})
		}
	}
	return input, strings.Join(system, "\n")
}

// convertResponsesUserContent keeps text and image parts, since Responses uses
// its own input_text/input_image part types.
func convertResponsesUserContent(content any) []any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": value}}
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := part["type"].(string)
			switch kind {
			case "text", "input_text":
				text, _ := part["text"].(string)
				if text != "" {
					out = append(out, map[string]any{"type": "input_text", "text": text})
				}
			case "image_url", "input_image":
				url, detail := imageURLValue(part["image_url"])
				if kind == "input_image" {
					url, _ = part["image_url"].(string)
					detail, _ = part["detail"].(string)
				}
				if url == "" {
					continue
				}
				image := map[string]any{"type": "input_image", "image_url": url}
				if detail != "" {
					image["detail"] = detail
				}
				out = append(out, image)
			}
		}
		return out
	default:
		return nil
	}
}

// imageURLValue accepts both the chat object and Responses string forms.
func imageURLValue(raw any) (string, string) {
	switch value := raw.(type) {
	case string:
		return value, ""
	case map[string]any:
		url, _ := value["url"].(string)
		detail, _ := value["detail"].(string)
		return url, detail
	default:
		return "", ""
	}
}

// convertResponsesTools flattens chat function tools into Responses tools.
func convertResponsesTools(raw any) []any {
	tools, _ := raw.([]any)
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := tool["type"].(string); kind != "function" {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, flattenResponsesFunction(function))
	}
	return out
}

// convertLegacyFunctions handles the deprecated top-level functions field.
func convertLegacyFunctions(raw any) []any {
	functions, _ := raw.([]any)
	out := make([]any, 0, len(functions))
	for _, item := range functions {
		if function, ok := item.(map[string]any); ok {
			out = append(out, flattenResponsesFunction(function))
		}
	}
	return out
}

func flattenResponsesFunction(function map[string]any) map[string]any {
	tool := map[string]any{"type": "function"}
	for _, key := range []string{"name", "description", "parameters", "strict"} {
		if value, ok := function[key]; ok {
			tool[key] = value
		}
	}
	return tool
}

// convertResponsesToolChoice maps chat tool_choice values onto Responses.
func convertResponsesToolChoice(raw any) any {
	switch value := raw.(type) {
	case string:
		return value
	case map[string]any:
		if kind, _ := value["type"].(string); kind == "function" {
			if function, ok := value["function"].(map[string]any); ok {
				if name, _ := function["name"].(string); name != "" {
					return map[string]any{"type": "function", "name": name}
				}
			}
		}
	}
	return nil
}

func convertLegacyFunctionCall(raw any) any {
	switch value := raw.(type) {
	case string:
		return value
	case map[string]any:
		if name, _ := value["name"].(string); name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

// convertResponsesText maps chat response_format/verbosity onto text.
func convertResponsesText(raw map[string]any) map[string]any {
	text := map[string]any{}
	if value, ok := raw["verbosity"]; ok {
		text["verbosity"] = value
	}
	if format, ok := raw["response_format"].(map[string]any); ok {
		switch kind, _ := format["type"].(string); kind {
		case "json_object":
			text["format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			if schema, ok := format["json_schema"].(map[string]any); ok {
				converted := map[string]any{"type": "json_schema"}
				for _, key := range []string{"name", "description", "schema", "strict"} {
					if value, ok := schema[key]; ok {
						converted[key] = value
					}
				}
				text["format"] = converted
			}
		}
	}
	if len(text) == 0 {
		return nil
	}
	return text
}

// convertResponsesReasoning maps reasoning_effort onto a reasoning object.
func convertResponsesReasoning(raw map[string]any) map[string]any {
	if value, ok := raw["reasoning"].(map[string]any); ok {
		return value
	}
	if effort, ok := raw["reasoning_effort"]; ok && effort != nil {
		return map[string]any{"effort": effort}
	}
	return nil
}

// toolCallsFromMessage extracts assistant tool calls from a chat message.
func toolCallsFromMessage(message map[string]any) []ToolCall {
	raw, _ := message["tool_calls"].([]any)
	out := make([]ToolCall, 0, len(raw))
	for _, item := range raw {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		out = append(out, ToolCall{
			ID:   stringValue(call["id"]),
			Type: stringValue(call["type"]),
			Function: ToolFunction{
				Name:      stringValue(function["name"]),
				Arguments: argumentsString(function["arguments"]),
			},
		})
	}
	return out
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// Chat performs a non-streaming Responses request and maps it back to a chat
// completion.
func (p *Responses) Chat(ctx context.Context, request Request) (*Result, error) {
	payload, err := json.Marshal(p.toResponsesBody(request, false))
	if err != nil {
		return nil, err
	}
	response, err := util.FetchUpstream(ctx, p.endpoint("/responses"), util.FetchOptions{
		Method:    http.MethodPost,
		Headers:   p.headers(request),
		Body:      bytes.NewReader(payload),
		TimeoutMs: p.cfg.RequestTimeoutMs,
	})
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &util.UpstreamStatusError{Status: response.StatusCode, Body: truncate(string(body), 500)}
	}

	var parsed responsesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, &util.UpstreamStatusError{Status: http.StatusBadGateway, Body: parsed.Error.Message}
	}

	text, reasoning, calls, finish := parsed.chatResult()
	message := Message{Role: "assistant"}
	if text != "" || len(calls) == 0 {
		message.Content = &text
	}
	if reasoning != "" {
		message.ReasoningContent = &reasoning
	}
	message.ToolCalls = calls

	result := &Result{
		ID:           parsed.ID,
		Created:      parsed.CreatedAt,
		Model:        parsed.Model,
		Message:      message,
		FinishReason: finish,
		Usage:        parsed.chatUsage(),
		Headers:      util.FilterResponseHeaders(response.Header),
	}
	if result.ID == "" {
		result.ID = "chatcmpl-responses-" + randomSuffix()
	}
	if result.Model == "" {
		result.Model = request.Model
	}
	if result.Created == 0 {
		result.Created = time.Now().Unix()
	}
	return result, nil
}

// responsesResponse is the subset of a Responses object the gateway needs.
type responsesResponse struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	CreatedAt         int64  `json:"created_at"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	Output            []any  `json:"output"`
	OutputText        string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *responsesUsage `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// chatResult flattens the output items into chat text, reasoning and tool calls.
func (r *responsesResponse) chatResult() (string, string, []ToolCall, *string) {
	var text, reasoning strings.Builder
	calls := []ToolCall{}

	for _, raw := range r.Output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch kind, _ := item["type"].(string); kind {
		case "message":
			parts, _ := item["content"].([]any)
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				switch partType, _ := part["type"].(string); partType {
				case "output_text":
					text.WriteString(stringValue(part["text"]))
				case "refusal":
					text.WriteString(stringValue(part["refusal"]))
				}
			}
		case "function_call":
			callID := stringValue(item["call_id"])
			if callID == "" {
				callID = stringValue(item["id"])
			}
			name := stringValue(item["name"])
			if callID == "" || name == "" {
				continue
			}
			calls = append(calls, ToolCall{
				ID:   callID,
				Type: "function",
				Function: ToolFunction{
					Name:      name,
					Arguments: argumentsString(item["arguments"]),
				},
			})
		case "reasoning":
			reasoning.WriteString(reasoningText(item))
		}
	}
	if text.Len() == 0 && r.OutputText != "" {
		text.WriteString(r.OutputText)
	}

	finish := "stop"
	switch {
	case len(calls) > 0:
		finish = "tool_calls"
	case r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens":
		finish = "length"
	}
	return text.String(), reasoning.String(), calls, &finish
}

func (r *responsesResponse) chatUsage() *Usage {
	if r.Usage == nil {
		return nil
	}
	total := r.Usage.TotalTokens
	if total == 0 {
		total = r.Usage.InputTokens + r.Usage.OutputTokens
	}
	return &Usage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      total,
	}
}

func reasoningText(item map[string]any) string {
	var builder strings.Builder
	for _, key := range []string{"summary", "content"} {
		parts, _ := item[key].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := part["type"].(string); kind {
			case "summary_text", "reasoning_text":
				builder.WriteString(stringValue(part["text"]))
			}
		}
	}
	return builder.String()
}

func argumentsString(raw any) string {
	switch value := raw.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// ChatStream performs a streaming Responses request and maps the event stream
// back to OpenAI chat-completion chunks.
func (p *Responses) ChatStream(ctx context.Context, request Request) (*http.Response, error) {
	payload, err := json.Marshal(p.toResponsesBody(request, true))
	if err != nil {
		return nil, err
	}
	upstream, err := util.FetchUpstream(ctx, p.endpoint("/responses"), util.FetchOptions{
		Method:    http.MethodPost,
		Headers:   p.headers(request),
		Body:      bytes.NewReader(payload),
		TimeoutMs: 0,
	})
	if err != nil {
		return nil, err
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body := util.ReadBodyLimited(upstream, 500)
		return nil, &util.UpstreamStatusError{Status: upstream.StatusCode, Body: body}
	}

	id := "chatcmpl-responses-" + randomSuffix()
	model := request.Model
	created := time.Now().Unix()
	roleSent := false
	textSent := false
	toolIndexes := map[string]int{}
	toolArguments := map[string]string{}
	reasoningText := map[string]string{}
	hasToolCall := false
	promptTokens, completionTokens, totalTokens := 0, 0, 0

	pipe := util.NewBufferedPipe()
	go func() {
		defer upstream.Body.Close()
		defer pipe.Close()

		send := func(payload any) error {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			_, err = pipe.Write([]byte("data: " + string(encoded) + "\n\n"))
			return err
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": id, "object": "chat.completion.chunk",
				"created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		sendRole := func() {
			if roleSent {
				return
			}
			roleSent = true
			_ = send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		}
		sendText := func(value string) {
			if value == "" {
				return
			}
			textSent = true
			sendRole()
			_ = send(chunk(map[string]any{"content": value}, nil))
		}
		sendReasoning := func(key, value string) {
			if value == "" {
				return
			}
			sendRole()
			reasoningText[key] += value
			_ = send(chunk(map[string]any{"reasoning_content": value}, nil))
		}
		finishReasoning := func(key, value string) {
			if value == "" {
				return
			}
			accumulated := reasoningText[key]
			switch {
			case accumulated == "":
				sendReasoning(key, value)
			case strings.HasPrefix(value, accumulated):
				sendReasoning(key, value[len(accumulated):])
			}
		}
		sendToolCall := func(itemID, callID, name, arguments string) {
			sendRole()
			index, ok := toolIndexes[itemID]
			if !ok {
				index = len(toolIndexes)
				toolIndexes[itemID] = index
			}
			tool := map[string]any{"index": index}
			if callID != "" {
				tool["id"] = callID
				tool["type"] = "function"
			}
			function := map[string]any{}
			if name != "" {
				function["name"] = name
			}
			if arguments != "" {
				function["arguments"] = arguments
				toolArguments[itemID] += arguments
			}
			if len(function) > 0 {
				tool["function"] = function
			}
			hasToolCall = true
			_ = send(chunk(map[string]any{"tool_calls": []any{tool}}, nil))
		}
		sendToolDelta := func(itemID, delta string) {
			if delta == "" {
				return
			}
			index, ok := toolIndexes[itemID]
			if !ok {
				return
			}
			toolArguments[itemID] += delta
			_ = send(chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index, "function": map[string]any{"arguments": delta},
				}},
			}, nil))
		}
		finish := func(reason string) {
			usage := map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
				"total_tokens":      totalTokens,
			}
			_ = send(map[string]any{
				"id": id, "object": "chat.completion.chunk",
				"created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
				"usage":   usage,
			})
		}

		parser := util.NewSSEParser(func(event util.SSEEvent) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
				return
			}
			kind, _ := parsed["type"].(string)
			switch kind {
			case "response.created":
				if response, ok := parsed["response"].(map[string]any); ok {
					if value, _ := response["id"].(string); value != "" {
						id = value
					}
					if value, _ := response["model"].(string); value != "" {
						model = value
					}
					if value := intFromAny(response["created_at"]); value > 0 {
						created = int64(value)
					}
				}
			case "response.output_item.added":
				item, _ := parsed["item"].(map[string]any)
				if itemType, _ := item["type"].(string); itemType == "function_call" {
					itemID, _ := item["id"].(string)
					sendToolCall(itemID, stringValue(item["call_id"]), stringValue(item["name"]), argumentsString(item["arguments"]))
				}
			case "response.output_text.delta":
				sendText(stringValue(parsed["delta"]))
			case "response.refusal.delta":
				sendText(stringValue(parsed["delta"]))
			case "response.reasoning_text.delta", "response.reasoning.delta", "response.reasoning_summary_text.delta":
				sendReasoning(reasoningKey(parsed), stringValue(parsed["delta"]))
			case "response.reasoning_text.done", "response.reasoning.done", "response.reasoning_summary_text.done":
				finishReasoning(reasoningKey(parsed), stringValue(parsed["text"]))
			case "response.function_call_arguments.delta":
				sendToolDelta(stringValue(parsed["item_id"]), stringValue(parsed["delta"]))
			case "response.function_call_arguments.done":
				itemID := stringValue(parsed["item_id"])
				final := stringValue(parsed["arguments"])
				accumulated := toolArguments[itemID]
				switch {
				case final == "":
				case strings.HasPrefix(final, accumulated):
					sendToolDelta(itemID, final[len(accumulated):])
				case accumulated == "":
					sendToolDelta(itemID, final)
				}
			case "response.output_item.done":
				item, _ := parsed["item"].(map[string]any)
				if itemType, _ := item["type"].(string); itemType == "function_call" {
					itemID, _ := item["id"].(string)
					if _, ok := toolIndexes[itemID]; !ok {
						sendToolCall(itemID, stringValue(item["call_id"]), stringValue(item["name"]), argumentsString(item["arguments"]))
					}
				}
			case "response.completed", "response.incomplete":
				response, _ := parsed["response"].(map[string]any)
				if value, _ := response["model"].(string); value != "" {
					model = value
				}
				if value, _ := response["id"].(string); value != "" {
					id = value
				}
				if output, ok := response["output"].([]any); ok {
					if !textSent {
						if text := responsesOutputText(output); text != "" {
							sendText(text)
						}
					}
					for _, raw := range output {
						item, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						if itemType, _ := item["type"].(string); itemType == "function_call" {
							itemID, _ := item["id"].(string)
							if _, ok := toolIndexes[itemID]; !ok {
								sendToolCall(itemID, stringValue(item["call_id"]), stringValue(item["name"]), argumentsString(item["arguments"]))
							}
						}
					}
				}
				if usage, ok := response["usage"].(map[string]any); ok {
					promptTokens = intFromAny(usage["input_tokens"])
					completionTokens = intFromAny(usage["output_tokens"])
					totalTokens = intFromAny(usage["total_tokens"])
					if totalTokens == 0 {
						totalTokens = promptTokens + completionTokens
					}
				}
				reason := "stop"
				if hasToolCall {
					reason = "tool_calls"
				}
				if details, ok := response["incomplete_details"].(map[string]any); ok {
					if details["reason"] == "max_output_tokens" {
						reason = "length"
					}
				}
				sendRole()
				finish(reason)
				_, _ = pipe.Write([]byte("data: [DONE]\n\n"))
			case "response.failed", "error":
				message := responsesStreamError(parsed)
				_ = send(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
			}
		})

		buffer := make([]byte, 32*1024)
		for {
			n, err := upstream.Body.Read(buffer)
			if n > 0 {
				parser.Push(string(buffer[:n]))
			}
			if err != nil {
				break
			}
		}
		parser.Flush()
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		Body: pipe,
	}, nil
}

func reasoningKey(event map[string]any) string {
	itemID := stringValue(event["item_id"])
	index := intFromAny(event["summary_index"])
	return itemID + ":" + strconv.Itoa(index)
}

func responsesOutputText(output []any) string {
	var builder strings.Builder
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := item["type"].(string); itemType != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch partType, _ := part["type"].(string); partType {
			case "output_text":
				builder.WriteString(stringValue(part["text"]))
			case "refusal":
				builder.WriteString(stringValue(part["refusal"]))
			}
		}
	}
	return builder.String()
}

func responsesStreamError(event map[string]any) string {
	if message := stringValue(event["message"]); message != "" {
		return message
	}
	if detail, ok := event["error"].(map[string]any); ok {
		if message := stringValue(detail["message"]); message != "" {
			return message
		}
	}
	if response, ok := event["response"].(map[string]any); ok {
		if detail, ok := response["error"].(map[string]any); ok {
			if message := stringValue(detail["message"]); message != "" {
				return message
			}
		}
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return "upstream stream error"
	}
	return string(encoded)
}
