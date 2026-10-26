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
//
// Templates are parsed once (Template) and rendered per request: parsing a
// dotted path with the regexes below on every relay request was measurable
// overhead for configs that use request/response templates.

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

// lookupPath resolves a pre-parsed dotted path against a decoded-JSON context.
// A missing value and a JSON null both come back as nil.
func lookupPath(context any, path []templateSegment) any {
	current := context
	for _, segment := range path {
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

type templateNodeKind uint8

const (
	templateScalar templateNodeKind = iota
	templateLiteral
	templateExact
	templateInterpolate
	templateArray
	templateObject
)

type templatePart struct {
	literal string
	path    []templateSegment
	isPath  bool
}

type templateNode struct {
	kind   templateNodeKind
	scalar any
	text   string
	path   []templateSegment
	parts  []templatePart
	items  []*templateNode
	fields map[string]*templateNode
}

// Template is a parsed template ready for repeated rendering.
type Template struct {
	root *templateNode
}

// ParseTemplate pre-parses a decoded JSON template value.
func ParseTemplate(value any) *Template {
	return &Template{root: buildTemplateNode(value)}
}

func buildTemplateNode(value any) *templateNode {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if match := exactPattern.FindStringSubmatch(trimmed); match != nil {
			return &templateNode{kind: templateExact, path: parseTemplatePath(match[1])}
		}
		if !strings.Contains(typed, "{{") {
			return &templateNode{kind: templateLiteral, text: typed}
		}
		return &templateNode{kind: templateInterpolate, parts: splitTemplateParts(typed)}
	case []any:
		items := make([]*templateNode, len(typed))
		for index, item := range typed {
			items[index] = buildTemplateNode(item)
		}
		return &templateNode{kind: templateArray, items: items}
	case map[string]any:
		fields := make(map[string]*templateNode, len(typed))
		for key, item := range typed {
			fields[key] = buildTemplateNode(item)
		}
		return &templateNode{kind: templateObject, fields: fields}
	default:
		return &templateNode{kind: templateScalar, scalar: value}
	}
}

func splitTemplateParts(template string) []templatePart {
	matches := placeholderPattern.FindAllStringSubmatchIndex(template, -1)
	if len(matches) == 0 {
		return []templatePart{{literal: template}}
	}
	parts := make([]templatePart, 0, len(matches)*2+1)
	last := 0
	for _, match := range matches {
		if match[0] > last {
			parts = append(parts, templatePart{literal: template[last:match[0]]})
		}
		parts = append(parts, templatePart{
			path:   parseTemplatePath(template[match[2]:match[3]]),
			isPath: true,
		})
		last = match[1]
	}
	if last < len(template) {
		parts = append(parts, templatePart{literal: template[last:]})
	}
	return parts
}

// Dynamic reports whether rendering depends on the context. A template with no
// placeholders renders to a constant value.
func (t *Template) Dynamic() bool {
	return t != nil && templateNodeDynamic(t.root)
}

func templateNodeDynamic(node *templateNode) bool {
	if node == nil {
		return false
	}
	switch node.kind {
	case templateExact:
		return true
	case templateInterpolate:
		for _, part := range node.parts {
			if part.isPath {
				return true
			}
		}
	case templateArray:
		for _, item := range node.items {
			if templateNodeDynamic(item) {
				return true
			}
		}
	case templateObject:
		for _, item := range node.fields {
			if templateNodeDynamic(item) {
				return true
			}
		}
	}
	return false
}

// Render renders the template against a decoded-JSON context.
func (t *Template) Render(context any) any {
	if t == nil || t.root == nil {
		return nil
	}
	return renderTemplateNode(t.root, context, nil)
}

// RenderAndFindUnresolved renders once and reports placeholder paths that are
// still present in the output (for example a context value that itself contains
// {{...}}). It replaces the previous render-twice FindUnresolved path.
func (t *Template) RenderAndFindUnresolved(context any) (any, []string) {
	if t == nil || t.root == nil {
		return nil, nil
	}
	var unresolved []string
	value := renderTemplateNode(t.root, context, &unresolved)
	return value, unresolved
}

func renderTemplateNode(node *templateNode, context any, unresolved *[]string) any {
	switch node.kind {
	case templateScalar:
		return node.scalar
	case templateLiteral:
		return node.text
	case templateExact:
		value := lookupPath(context, node.path)
		if unresolved != nil {
			collectUnresolvedValue(value, unresolved)
		}
		return value
	case templateInterpolate:
		var builder strings.Builder
		for _, part := range node.parts {
			if !part.isPath {
				builder.WriteString(part.literal)
				continue
			}
			writeTemplateValue(&builder, lookupPath(context, part.path))
		}
		rendered := builder.String()
		if unresolved != nil && strings.Contains(rendered, "{{") {
			collectUnresolvedString(rendered, unresolved)
		}
		return rendered
	case templateArray:
		out := make([]any, len(node.items))
		for index, item := range node.items {
			out[index] = renderTemplateNode(item, context, unresolved)
		}
		return out
	case templateObject:
		out := make(map[string]any, len(node.fields))
		for key, item := range node.fields {
			out[key] = renderTemplateNode(item, context, unresolved)
		}
		return out
	}
	return nil
}

func writeTemplateValue(builder *strings.Builder, value any) {
	switch typed := value.(type) {
	case nil:
	case string:
		builder.WriteString(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err == nil {
			builder.Write(encoded)
		}
	}
}

// RenderTemplate deep-renders a JSON template structure against a context. A
// string that is exactly one placeholder resolves to the raw value (preserving
// numbers/objects); otherwise string interpolation applies.
//
// Hot paths should ParseTemplate once and call Template.Render instead.
func RenderTemplate(template any, context any) any {
	return ParseTemplate(template).Render(context)
}

// FindUnresolved returns the placeholder paths still present after rendering,
// which the forwarder turns into a 400/502 rather than sending {{...}} upstream.
func FindUnresolved(template any, context any) []string {
	_, unresolved := ParseTemplate(template).RenderAndFindUnresolved(context)
	return unresolved
}

func collectUnresolvedValue(value any, out *[]string) {
	switch typed := value.(type) {
	case string:
		if strings.Contains(typed, "{{") {
			collectUnresolvedString(typed, out)
		}
	case []any:
		for _, item := range typed {
			collectUnresolvedValue(item, out)
		}
	case map[string]any:
		for _, item := range typed {
			collectUnresolvedValue(item, out)
		}
	}
}

func collectUnresolvedString(value string, out *[]string) {
	for _, match := range placeholderPattern.FindAllStringSubmatch(value, -1) {
		if len(match) > 1 && match[1] != "" {
			*out = append(*out, match[1])
		}
	}
}
