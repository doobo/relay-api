package util

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

type jsonEdit struct {
	start int
	end   int
	data  []byte
}

// RewriteTopLevelJSON replaces or inserts fields on a JSON object while
// preserving every other byte. Streaming the body through a decoder lets the
// relay pin model/stream without decoding the request into map[string]any and
// re-encoding the whole payload.
func RewriteTopLevelJSON(raw []byte, fields map[string]json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, errors.New("json object expected")
	}
	openEnd := int(decoder.InputOffset())

	spans := make(map[string]jsonEdit, len(fields))
	fieldCount := 0
	for decoder.More() {
		fieldCount++
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("json object key expected")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		end := int(decoder.InputOffset())
		if replacement, wanted := fields[key]; wanted {
			spans[key] = jsonEdit{start: end - len(value), end: end, data: replacement}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}

	edits := make([]jsonEdit, 0, len(fields))
	missing := make([]string, 0, len(fields))
	for key := range fields {
		if span, ok := spans[key]; ok {
			edits = append(edits, span)
		} else {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		pairs := make([]byte, 0, len(missing)*16)
		for index, key := range missing {
			if index > 0 {
				pairs = append(pairs, ',')
			}
			quotedKey, err := json.Marshal(key)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, quotedKey...)
			pairs = append(pairs, ':')
			pairs = append(pairs, fields[key]...)
		}
		if fieldCount > 0 {
			pairs = append(pairs, ',')
		}
		edits = append(edits, jsonEdit{start: openEnd, end: openEnd, data: pairs})
	}

	sort.Slice(edits, func(left, right int) bool {
		return edits[left].start < edits[right].start
	})
	size := len(raw)
	for _, edit := range edits {
		size += len(edit.data) - (edit.end - edit.start)
	}
	var output bytes.Buffer
	output.Grow(size)
	last := 0
	for _, edit := range edits {
		if edit.start < last {
			return nil, errors.New("overlapping json edits")
		}
		output.Write(raw[last:edit.start])
		output.Write(edit.data)
		last = edit.end
	}
	output.Write(raw[last:])
	return output.Bytes(), nil
}
