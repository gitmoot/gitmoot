package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTitle(t *testing.T) {
	sixty := strings.Repeat("a", 60)
	tests := []struct {
		name, content, want string
	}{
		{name: "plain sentence", content: "Deployment uses the canary lane", want: "Deployment uses the canary lane"},
		{name: "first sentence", content: "Deployment uses canaries. Rollback is automatic.", want: "Deployment uses canaries"},
		{name: "word boundary truncation", content: "The deployment process always verifies production health before moving traffic to the newest release", want: "The deployment process always verifies production health…"},
		{name: "heading", content: "# Deployment process", want: "Deployment process"},
		{name: "list bullet", content: "- Deployment process", want: "Deployment process"},
		{name: "ordered list", content: "12) Deployment process", want: "Deployment process"},
		{name: "blockquote", content: "> Deployment process", want: "Deployment process"},
		{name: "bold label", content: "**Why:** Deployment process", want: "Deployment process"},
		{name: "underscore bold label", content: "__How to apply:__ Use the canary lane", want: "Use the canary lane"},
		{name: "plain label stays", content: "Note: Deployment process", want: "Note: Deployment process"},
		{name: "inline markup", content: "Use `git status` with [the guide](https://example.com) and ![diagram](diagram.png) for **safe** _deploys_", want: "Use git status with the guide and diagram for safe deploys"},
		{name: "frontmatter", content: "---\nrepo: acme/widget\n---\n\n## Deployment process", want: "Deployment process"},
		{name: "all code", content: "```go\nfmt.Println(\"hello\")\n```", want: ""},
		{name: "empty", content: " \n\t ", want: ""},
		{name: "slug hard cut", content: strings.Repeat("a", 70), want: sixty + "…"},
		{name: "multibyte rune boundary", content: strings.Repeat("界", 61), want: strings.Repeat("界", 60) + "…"},
		{name: "exact boundary", content: sixty, want: sixty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Title(tc.content)
			if got != tc.want {
				t.Fatalf("Title(%q) = %q, want %q", tc.content, got, tc.want)
			}
			if gotAgain := Title(tc.content); gotAgain != got {
				t.Fatalf("Title is not deterministic: first %q, second %q", got, gotAgain)
			}
			if tc.name == "multibyte rune boundary" && !utf8.ValidString(got) {
				t.Fatalf("Title returned invalid UTF-8: %q", got)
			}
		})
	}
}
