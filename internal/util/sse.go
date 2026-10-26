package util

import "bytes"

var (
	lfSeparator   = []byte("\n\n")
	crlfSeparator = []byte("\r\n\r\n")
	eventField    = []byte("event")
	dataField     = []byte("data")
)

// SSEEvent is one parsed server-sent event.
type SSEEvent struct {
	Event string
	Data  string
}

// SSEParser incrementally parses server-sent events from arbitrary byte chunks
// (port of utils/sse.ts). Parsed events are delivered to the handler as they
// complete. The internal buffer is byte-based so the streaming hot path does
// not allocate a string for every upstream read.
type SSEParser struct {
	buffer  []byte
	onEvent func(SSEEvent)
	// keep, when set, filters raw events before line parsing. Callers that only
	// care about a few event types (chat usage, Responses completion) skip the
	// parsing and string allocation for every other chunk.
	keep func([]byte) bool
}

// NewSSEParser creates a parser delivering complete events to onEvent.
func NewSSEParser(onEvent func(SSEEvent)) *SSEParser {
	return &SSEParser{onEvent: onEvent}
}

// NewFilteredSSEParser creates a parser that only decodes events whose raw
// bytes satisfy keep. It avoids per-event parsing for chunks of no interest.
func NewFilteredSSEParser(onEvent func(SSEEvent), keep func([]byte) bool) *SSEParser {
	return &SSEParser{onEvent: onEvent, keep: keep}
}

// Push feeds a raw text chunk; any complete events are emitted immediately.
func (p *SSEParser) Push(text string) {
	p.PushBytes([]byte(text))
}

// PushBytes feeds a raw byte chunk; any complete events are emitted
// immediately. The chunk is not retained after the call.
func (p *SSEParser) PushBytes(chunk []byte) {
	p.buffer = append(p.buffer, chunk...)
	consumed := 0
	for {
		index, length := findSeparator(p.buffer[consumed:])
		if index == -1 {
			break
		}
		raw := p.buffer[consumed : consumed+index]
		consumed += index + length
		if p.keep != nil && !p.keep(raw) {
			continue
		}
		if event, ok := parseSSEEvent(raw); ok {
			p.onEvent(event)
		}
	}
	if consumed > 0 {
		remaining := copy(p.buffer, p.buffer[consumed:])
		p.buffer = p.buffer[:remaining]
	}
}

// Flush processes a trailing event without a final blank line.
func (p *SSEParser) Flush() {
	if len(bytes.TrimSpace(p.buffer)) != 0 && (p.keep == nil || p.keep(p.buffer)) {
		if event, ok := parseSSEEvent(p.buffer); ok {
			p.onEvent(event)
		}
	}
	p.buffer = p.buffer[:0]
}

// findSeparator locates the next blank-line event separator, returning its
// index and length, or -1 when none is present yet.
func findSeparator(buffer []byte) (int, int) {
	lf := bytes.Index(buffer, lfSeparator)
	crlf := bytes.Index(buffer, crlfSeparator)
	if lf == -1 && crlf == -1 {
		return -1, 0
	}
	if crlf != -1 && (lf == -1 || crlf+2 < lf) {
		return crlf, 4
	}
	return lf, 2
}

func parseSSEEvent(raw []byte) (SSEEvent, bool) {
	var (
		event   string
		data    []byte
		hasData bool
	)
	for len(raw) > 0 {
		line := raw
		if index := bytes.IndexByte(raw, '\n'); index >= 0 {
			line = raw[:index]
			raw = raw[index+1:]
		} else {
			raw = nil
		}
		if length := len(line); length > 0 && line[length-1] == '\r' {
			line = line[:length-1]
		}
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field := line
		var value []byte
		if colon := bytes.IndexByte(line, ':'); colon >= 0 {
			field = line[:colon]
			value = line[colon+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch {
		case bytes.Equal(field, eventField):
			event = string(value)
		case bytes.Equal(field, dataField):
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		}
	}
	if !hasData {
		return SSEEvent{}, false
	}
	return SSEEvent{Event: event, Data: string(data)}, true
}
