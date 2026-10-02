package util

import "testing"

func TestMatchConfigPattern(t *testing.T) {
	cases := []struct {
		name, path, wantRest string
		want                 bool
	}{
		{"weather", "weather", "", true},
		{"weather", "weather/x", "", false},
		{"api/*", "api/a", "a", true},
		{"api/*", "api/a/b", "", false},
		{"api/**", "api/a", "a", true},
		{"api/**", "api/a/b", "a/b", true},
		{"api/**", "api", "", false},
		{"a/*/c", "a/b/c", "b/c", true},
	}
	for _, testCase := range cases {
		rest, ok := MatchConfigPattern(testCase.name, testCase.path)
		if ok != testCase.want || rest != testCase.wantRest {
			t.Errorf("MatchConfigPattern(%q,%q) = (%q,%v), want (%q,%v)",
				testCase.name, testCase.path, rest, ok, testCase.wantRest, testCase.want)
		}
	}
}

func TestComparePatternSpecificity(t *testing.T) {
	// `api/*` is more specific than `api/**`.
	if ComparePatternSpecificity("api/*", "api/**") <= 0 {
		t.Fatal("api/* should beat api/**")
	}
	// A longer literal prefix wins.
	if ComparePatternSpecificity("a/b/*", "a/**") <= 0 {
		t.Fatal("a/b/* should beat a/**")
	}
}

func TestRenderTemplate(t *testing.T) {
	context := map[string]any{
		"body": map[string]any{"city": "Hangzhou"},
		"messages": []any{
			map[string]any{"content": "old"},
			map[string]any{"content": "new"},
		},
	}

	if got := RenderTemplate("{{body.city}}", context); got != "Hangzhou" {
		t.Fatalf("exact placeholder = %v", got)
	}
	if got := RenderTemplate("Hi {{body.city}}!", context); got != "Hi Hangzhou!" {
		t.Fatalf("interpolation = %v", got)
	}
	if got := RenderTemplate("{{messages[-1].content}}", context); got != "new" {
		t.Fatalf("negative index = %v", got)
	}
	if got := RenderTemplate(map[string]any{"city": "{{body.city}}"}, context); got.(map[string]any)["city"] != "Hangzhou" {
		t.Fatalf("nested render = %v", got)
	}
	if got := RenderTemplate("{{missing.value}}", context); got != nil {
		t.Fatalf("missing exact placeholder = %v", got)
	}

	// Reference parity: renderString substitutes unknown placeholders with "",
	// so scanning the rendered output finds nothing left unresolved.
	if unresolved := FindUnresolved(map[string]any{"a": "{{nope}}", "b": "{{body.city}}"}, context); len(unresolved) != 0 {
		t.Fatalf("unresolved = %v", unresolved)
	}
}
