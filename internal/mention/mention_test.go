package mention

import (
	"reflect"
	"testing"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"@codex-b":   "codex-b",
		"  @helper ": "helper",
		"builder":    "builder",
		"@":          "",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Fatalf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"@codex-b can you inspect the adapter? @researcher compare options", []string{"codex-b", "researcher"}},
		{"thanks @helper, and @helper again", []string{"helper"}}, // dedupe + trailing comma
		{"no mentions here", nil},
		{"a lone @ is not a mention", nil},
		{"punctuation @a. @b! @c) done", []string{"a", "b", "c"}},
		{"email jerry@example.com is not a mention", nil}, // interior @ (no leading @)
	}
	for _, c := range cases {
		got := Parse(c.body)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("Parse(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
