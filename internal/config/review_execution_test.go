package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadReviewExecutionPrecedenceAndFailClosed pins the #2364 policy
// contract: only a repository section can declare execution = "none", it binds
// only that repository, and every unreadable declaration refuses rather than
// allowing execution.
func TestLoadReviewExecutionPrecedenceAndFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		repo    string
		want    string
		wantErr string
	}{
		{name: "missing config allows", body: "", repo: "o/r", want: ReviewExecutionAllowed},
		{name: "undeclared repo allows", body: "[repos.\"o/other\".review]\nexecution = \"none\"\n", repo: "o/r", want: ReviewExecutionAllowed},
		{name: "declared none binds its repo", body: "[repos.\"o/r\".review]\nexecution = \"none\"\n", repo: "o/r", want: ReviewExecutionNone},
		{name: "declared allowed", body: "[repos.\"o/r\".review]\nexecution = \"allowed\"\n", repo: "o/r", want: ReviewExecutionAllowed},
		{name: "unrelated bad field does not change the answer", body: "[repos.\"o/r\".review]\nexecution = \"none\"\nnative_fanout_enabled = maybe\n", repo: "o/r", want: ReviewExecutionNone},
		{name: "invalid value refuses its repo", body: "[repos.\"o/r\".review]\nexecution = \"nope\"\n", repo: "o/r", wantErr: "unsupported execution"},
		{name: "invalid value leaves other repos alone", body: "[repos.\"o/r\".review]\nexecution = \"nope\"\n", repo: "o/other", want: ReviewExecutionAllowed},
		{name: "global key refuses every repo", body: "[review]\nexecution = \"none\"\n", repo: "o/r", wantErr: "repository-scoped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := Paths{ConfigFile: filepath.Join(t.TempDir(), "missing.toml")}
			if tc.body != "" {
				paths = writeReviewConfig(t, tc.body)
			}
			got, err := LoadReviewExecution(paths, tc.repo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadReviewExecution = %q, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("LoadReviewExecution = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
