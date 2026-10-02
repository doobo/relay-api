package util

import "strings"

// SSEEvent is one parsed server-sent event.
type SSEEvent struct {
	Event string
	Data  string
}

// SSEParser incrementally parses server-sent events from arbitrary text chunks
// (port of utils/sse.ts). Parsed events are delivered to the handler as they
// complete.
type SSEParser struct {
	buffer  string
	onEvent func(SSEEvent)
}

// NewSSEParser creates a parser delivering complete events to onEvent.
func NewSSEParser(onEvent func(SSEEvent)) *SSEParser {
	return &SSEParser{onEvent: onEvent}
}

// Push feeds a raw text chunk; any complete events are emitted immediately.
func (p *SSEParser) Push(text string) {
	p.buffer += text
	for {
		index, length := findSeparator(p.buffer)
		if index == -1 {
			return
		}
		raw := p.buffer[:index]
		p.buffer = p.buffer[index+length:]
		if event, ok := parseSSEEvent(raw); ok {
			p.onEvent(event)
		}
	}
}

// Flush processes a trailing event without a final blank line.
func (p *SSEParser) Flush() {
	if strings.TrimSpace(p.buffer) != "" {
		if event, ok := parseSSEEvent(p.buffer); ok {
			p.onEvent(event)
		}
	}
	p.buffer = ""
}

// findSeparator locates the next blank-line event separator, returning its
// index and length, or -1 when none is present yet.
func findSeparator(buffer string) (int, int) {
	lf := strings.Index(buffer, "\n\n")
	crlf := strings.Index(buffer, "\r\n\r\n")
	if lf == -1 && crlf == -1 {
		return -1, 0
	}
	if crlf != -1 && (lf == -1 || crlf+2 < lf) {
		return crlf, 4
	}
	return lf, 2
}

func parseSSEEvent(raw string) (SSEEvent, bool) {
	var (
		event     string
		dataLines []string
	)
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSuffix(line, "\r")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		field := trimmed
		value := ""
		if colon := strings.Index(trimmed, ":"); colon != -1 {
			field = trimmed[:colon]
			value = trimmed[colon+1:]
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
		}
		switch field {
		case "event":
			event = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if len(dataLines) == 0 {
		return SSEEvent{}, false
	}
	return SSEEvent{Event: event, Data: strings.Join(dataLines, "\n")}, true
}
