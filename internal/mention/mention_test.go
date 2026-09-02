package mention

import "testing"

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
