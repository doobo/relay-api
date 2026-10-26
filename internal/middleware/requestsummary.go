package middleware

import (
	"bytes"
	"context"
	"encoding/json"
)

// RequestSummary is the small set of top-level request fields the gateway
// needs before forwarding: model permissions, client identity, chat parameters
// and the OpenAI fast-path check. It is decoded lazily without building a
// map[string]any, which keeps large nested bodies (messages, tools, images) out
// of the allocation path for plain passthrough requests.
type RequestSummary struct {
	Model       string
	Stream      bool
	StreamKnown bool
	HasMessages bool
	Temperature *float64
	MaxTokens   *int
	User        string
	UserID      string
}

// RequestSummaryFrom returns the lazily decoded summary, or ok=false when the
// body is empty, invalid JSON, or not a JSON object.
func RequestSummaryFrom(ctx context.Context) (RequestSummary, bool) {
	body, ok := ctx.Value(parsedBodyKey).(*parsedBody)
	if !ok {
		return RequestSummary{}, false
	}
	return body.requestSummary()
}

func (b *parsedBody) requestSummary() (RequestSummary, bool) {
	b.summaryOnce.Do(func() {
		raw := bytes.TrimSpace(b.raw)
		if len(raw) == 0 || raw[0] != '{' {
			return
		}
		var fields summaryFields
		if err := json.Unmarshal(raw, &fields); err != nil {
			return
		}
		b.summary = fields.summary()
		b.summaryOK = true
	})
	return b.summary, b.summaryOK
}

type summaryFields struct {
	Model       optionalString   `json:"model"`
	Stream      optionalBool     `json:"stream"`
	Messages    messagesPresence `json:"messages"`
	Temperature optionalNumber   `json:"temperature"`
	MaxTokens   optionalNumber   `json:"max_tokens"`
	User        optionalString   `json:"user"`
	Metadata    metadataFields   `json:"metadata"`
}

func (f summaryFields) summary() RequestSummary {
	summary := RequestSummary{
		Model:       f.Model.value,
		Stream:      f.Stream.value,
		StreamKnown: !f.Stream.present || f.Stream.valid,
		HasMessages: f.Messages.nonEmpty,
		User:        f.User.value,
		UserID:      f.Metadata.UserID.value,
	}
	if f.Temperature.set {
		value := f.Temperature.value
		summary.Temperature = &value
	}
	if f.MaxTokens.set {
		value := int(f.MaxTokens.value)
		summary.MaxTokens = &value
	}
	return summary
}

// optionalString accepts a JSON string and silently ignores other types, which
// mirrors the previous map[string]any type assertions.
type optionalString struct {
	value string
	set   bool
}

func (s *optionalString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	var value string
	if json.Unmarshal(trimmed, &value) == nil {
		s.value = value
		s.set = true
	}
	return nil
}

// optionalBool behaves like optionalString for booleans. present and valid are
// tracked separately so an invalid stream value ("yes", null) is not mistaken
// for an absent (false) flag by the raw-body fast path.
type optionalBool struct {
	value   bool
	present bool
	valid   bool
}

func (b *optionalBool) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, jsonNull) {
		b.present = true
		return nil
	}
	var value bool
	if json.Unmarshal(trimmed, &value) == nil {
		b.value = value
		b.present = true
		b.valid = true
	} else {
		b.present = true
	}
	return nil
}

// optionalNumber accepts a JSON number; non-numbers are ignored.
type optionalNumber struct {
	value float64
	set   bool
}

func (n *optionalNumber) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	var value float64
	if json.Unmarshal(trimmed, &value) == nil {
		n.value = value
		n.set = true
	}
	return nil
}

// messagesPresence checks that messages is a non-empty array without copying
// or decoding its contents.
type messagesPresence struct {
	nonEmpty bool
}

func (m *messagesPresence) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	// The whole body was already syntax-checked, so the first non-space byte
	// after '[' is the closing bracket exactly when the array is empty.
	rest := bytes.TrimSpace(trimmed[1:])
	m.nonEmpty = len(rest) > 0 && rest[0] != ']'
	return nil
}

// metadataFields extracts metadata.user_id and ignores metadata values that are
// not objects.
type metadataFields struct {
	UserID optionalString `json:"user_id"`
}

func (m *metadataFields) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	type plain metadataFields
	var value plain
	if err := json.Unmarshal(trimmed, &value); err == nil {
		*m = metadataFields(value)
	}
	return nil
}

var jsonNull = []byte("null")
