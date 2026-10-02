package util

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Simple path-based template engine (port of transform/template.ts).
// Supports {{path.to.value}} placeholders resolved against a context object,
// plus {{messages[-1].content}} negative indexing. No code execution.

var (
	placeholderPattern = regexp.MustCompile(`\{\{\s*([\w.\[\]-]+)\s*\}\}`)
	exactPattern       = regexp.MustCompile(`^\{\{\s*([\w.\[\]-]+)\s*\}\}$`)
	indexedSegment     = regexp.MustCompile(`^(\w+)\[(-?\d+)\]$`)
	wordSegment        = regexp.MustCompile(`^\w+$`)
)

type templateSegment struct {
	key      string
	index    int
	hasIndex bool
}

func parseTemplatePath(path string) []templateSegment {
	parts := strings.Split(path, ".")
	out := make([]templateSegment, 0, len(parts))
	for _, part := range parts {
		if match := indexedSegment.FindStringSubmatch(part); match != nil {
			index, _ := strconv.Atoi(match[2])
			out = append(out, templateSegment{key: match[1], index: index, hasIndex: true})
			continue
		}
		if wordSegment.MatchString(part) {
			out = append(out, templateSegment{key: part})
		}
	}
	return out
}

// templateLookup resolves a dotted path against a decoded-JSON context. A
// missing value and a JSON null both come back as nil.
func templateLookup(context any, path string) any {
	current := context
	for _, segment := range parseTemplatePath(path) {
		if current == nil {
			return nil
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[segment.key]
		if segment.hasIndex {
			array, ok := current.([]any)
			if !ok {
				return nil
			}
			index := segment.index
			if index < 0 {
				index = len(array) + index
			}
			if index < 0 || index >= len(array) {
				return nil
			}
			current = array[index]
		}
	}
	return current
}

func renderTemplateString(template string, context any) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		sub := placeholderPattern.FindStringSubmatch(match)
		value := templateLookup(context, sub[1])
		switch typed := value.(type) {
		case nil:
			return ""
		case string:
			return typed
		default:
			encoded, err := json.Marshal(typed)
			if err != nil {
				return ""
			}
			return string(encoded)
		}
	})
}

// RenderTemplate deep-renders a JSON template structure against a context. A
// string that is exactly one placeholder resolves to the raw value (preserving
// numbers/objects); otherwise string interpolation applies.
func RenderTemplate(template any, context any) any {
	switch typed := template.(type) {
	case string:
		if match := exactPattern.FindStringSubmatch(strings.TrimSpace(typed)); match != nil {
			return templateLookup(context, match[1])
		}
		return renderTemplateString(typed, context)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = RenderTemplate(item, context)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[key] = RenderTemplate(value, context)
		}
		return out
	default:
		return template
	}
}

// FindUnresolved returns the placeholder paths still present after rendering,
// which the forwarder turns into a 400/502 rather than sending {{...}} upstream.
func FindUnresolved(template any, context any) []string {
	rendered := RenderTemplate(template, context)
	var unresolved []string
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			for _, match := range placeholderPattern.FindAllStringSubmatch(typed, -1) {
				if match[1] != "" {
					unresolved = append(unresolved, match[1])
				}
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(rendered)
	return unresolved
}
