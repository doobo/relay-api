package util

import (
	"bytes"
	"reflect"
	"testing"
)

func TestSSEParserChunked(t *testing.T) {
	var events []SSEEvent
	parser := NewSSEParser(func(event SSEEvent) {
		events = append(events, event)
	})
	for _, chunk := range []string{
		"data: he",
		"llo\n\n",
		"event: done\r\n",
		"data: a\r\ndata: b\r\n\r\n",
		": comment\n\n",
		"data: tail",
	} {
		parser.Push(chunk)
	}
	parser.Flush()

	want := []SSEEvent{
		{Data: "hello"},
		{Event: "done", Data: "a\nb"},
		{Data: "tail"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

func TestFilteredSSEParser(t *testing.T) {
	var events []SSEEvent
	parser := NewFilteredSSEParser(func(event SSEEvent) {
		events = append(events, event)
	}, func(raw []byte) bool {
		return bytes.Contains(raw, []byte("keep"))
	})
	parser.PushBytes([]byte("data: drop\n\ndata: keep\n\n"))

	if len(events) != 1 || events[0].Data != "keep" {
		t.Fatalf("events = %+v", events)
	}
}
