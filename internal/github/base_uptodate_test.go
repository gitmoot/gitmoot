package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// #1870 review finding 2 (P3): every other test reaches
// BaseRequiresUpToDateHead through the MergeGateGitHub interface fake, so the
// real client's gh-output handling - the jq filter and the swallow to
// UNDETERMINED - had no direct coverage. These drive GhClient itself, reusing
// the fakeRunner harness already used by sibling methods in this package.
func TestBaseRequiresUpToDateHead(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}

	t.Run("strict true is required and known", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: "true\n"}}}
		client := GhClient{Runner: runner}
		required, known, err := client.BaseRequiresUpToDateHead(context.Background(), repo, "main")
		if err != nil {
			t.Fatalf("returned error: %v", err)
		}
		if !required || !known {
			t.Fatalf("required=%v known=%v, want true/true", required, known)
		}
		args := strings.Join(runner.calls[0], " ")
		if !strings.Contains(args, "repos/o/r/branches/main/protection") {
			t.Fatalf("call args = %q, want the protection endpoint for the base branch", args)
		}
		if !strings.Contains(args, ".required_status_checks.strict") {
			t.Fatalf("call args = %q, want the strict jq filter", args)
		}
	})

	t.Run("strict false is known and not required", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: "false\n"}}}
		client := GhClient{Runner: runner}
		required, known, err := client.BaseRequiresUpToDateHead(context.Background(), repo, "main")
		if err != nil || required || !known {
			t.Fatalf("required=%v known=%v err=%v, want false/true/nil", required, known, err)
		}
	})

	// The merge gate merges a behind head only on known && !required, so any
	// answer that is not a clean "false" MUST come back undetermined.
	t.Run("undetermined answers never read as not-required", func(t *testing.T) {
		cases := []struct {
			name   string
			result subprocess.Result
			err    error
		}{
			{"404 unprotected branch", subprocess.Result{Stderr: "HTTP 404: Branch not protected"}, errors.New("exit status 1")},
			{"permission failure", subprocess.Result{Stderr: "HTTP 403: Resource not accessible by integration"}, errors.New("exit status 1")},
			{"empty stdout", subprocess.Result{Stdout: ""}, nil},
			{"null stdout", subprocess.Result{Stdout: "null\n"}, nil},
			{"unfiltered json", subprocess.Result{Stdout: "{\"strict\":true}\n"}, nil},
			// #1870 round-2 finding (P3): every case above pairs its error with
			// empty or non-determinate stdout, so the stdout switch alone could
			// satisfy them and the explicit error check survived deletion. These
			// two pair a gh FAILURE with determinate-looking output, so only the
			// error check can make them fail closed - gh writes partial or stale
			// stdout alongside a non-zero exit often enough to matter.
			{"error with determinate true stdout", subprocess.Result{Stdout: "true\n", Stderr: "HTTP 403: Resource not accessible by integration"}, errors.New("exit status 1")},
			{"error with determinate false stdout", subprocess.Result{Stdout: "false\n", Stderr: "HTTP 502: Bad Gateway"}, errors.New("exit status 1")},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				runner := &fakeRunner{results: []subprocess.Result{tc.result}}
				if tc.err != nil {
					runner.errs = []error{tc.err}
				}
				client := GhClient{Runner: runner, MaxRetries: 0}
				required, known, err := client.BaseRequiresUpToDateHead(context.Background(), repo, "main")
				if err != nil {
					t.Fatalf("returned error: %v", err)
				}
				if known {
					t.Fatalf("known = true for %q, want undetermined so the gate fails closed", tc.name)
				}
				if required {
					t.Fatalf("required = true with known=false for %q", tc.name)
				}
			})
		}
	})

	t.Run("missing repo or branch asks nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			repo   Repository
			branch string
		}{
			{"no repo", Repository{}, "main"},
			{"no branch", repo, "   "},
		} {
			t.Run(tc.name, func(t *testing.T) {
				runner := &fakeRunner{}
				client := GhClient{Runner: runner}
				_, known, err := client.BaseRequiresUpToDateHead(context.Background(), tc.repo, tc.branch)
				if err != nil {
					t.Fatalf("returned error: %v", err)
				}
				if known {
					t.Fatalf("known = true, want undetermined")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("calls = %v, want no gh invocation", runner.calls)
				}
			})
		}
	})
}
