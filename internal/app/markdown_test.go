package app

import "testing"

func TestMarkdownInlineEscapesUserText(t *testing.T) {
	cases := map[string]string{
		"Build the thing":  "Build the thing",
		"Write_docs *now*": "Write\\_docs \\*now\\*",
		"[draft] a+b":      "\\[draft\\] a\\+b",
		"path\\to":         "path\\\\to",
	}
	for in, want := range cases {
		if got := markdownInline(in); got != want {
			t.Errorf("markdownInline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkdownCodeHandlesBackticks(t *testing.T) {
	cases := map[string]string{
		"/tmp/project":      "`/tmp/project`",
		"/tmp/with`tick":    "`` /tmp/with`tick ``",
		"/tmp/with``double": "``` /tmp/with``double ```",
		"/tmp/line\nbreak":  "`/tmp/line break`",
	}
	for in, want := range cases {
		if got := markdownCode(in); got != want {
			t.Errorf("markdownCode(%q) = %q, want %q", in, got, want)
		}
	}
}
