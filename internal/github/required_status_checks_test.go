package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// #2605: the merge gate ignores a non-required action_required check run only
// when RequiredStatusCheckContexts KNOWS the required set, so the real client's
// parsing and its swallow to UNDETERMINED are driven directly here.
func TestRequiredStatusCheckContexts(t *testing.T) {
	repo := Repository{Owner: "o", Name: "r"}
	classic := `{"required_status_checks":{"strict":false,"contexts":["Symbol font is the upstream subset","HerdrKit and SwiftTerm (Linux)"],"checks":[{"context":"HerdrKit and SwiftTerm (Linux)","app_id":15368},{"context":"mac-evidence/org-ios-sim-build-v1","app_id":null}]}}`
	rules := `[{"type":"pull_request","parameters":{}},{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"ruleset-check"},{"context":"Symbol font is the upstream subset"}]}}]`

	t.Run("classic contexts, classic checks and rulesets are unioned", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: classic}, {Stdout: rules}}}
		client := GhClient{Runner: runner}
		contexts, known, err := client.RequiredStatusCheckContexts(context.Background(), repo, "release/v1")
		if err != nil || !known {
			t.Fatalf("known=%v err=%v, want known/nil", known, err)
		}
		want := []string{"Symbol font is the upstream subset", "HerdrKit and SwiftTerm (Linux)", "mac-evidence/org-ios-sim-build-v1", "ruleset-check"}
		if !reflect.DeepEqual(contexts, want) {
			t.Fatalf("contexts = %q, want %q", contexts, want)
		}
		if len(runner.calls) != 2 {
			t.Fatalf("calls = %v, want classic protection and rulesets reads", runner.calls)
		}
		if args := strings.Join(runner.calls[0], " "); !strings.Contains(args, "repos/o/r/branches/release%2Fv1/protection") {
			t.Fatalf("classic call = %q, want the escaped protection endpoint", args)
		}
		if args := strings.Join(runner.calls[1], " "); !strings.Contains(args, "repos/o/r/rules/branches/release%2Fv1") {
			t.Fatalf("rules call = %q, want the escaped rulesets endpoint", args)
		}
	})

	// #2605 review: gh api --paginate prints one JSON array per page. A
	// required check that only appears on page 2 must still be returned.
	t.Run("every rulesets page is read", func(t *testing.T) {
		pages := `[{"type":"pull_request","parameters":{}}]` + "\n" +
			`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"page-two-check"}]}}]`
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{}`}, {Stdout: pages}}}
		client := GhClient{Runner: runner}
		contexts, known, err := client.RequiredStatusCheckContexts(context.Background(), repo, "main")
		if err != nil || !known {
			t.Fatalf("known=%v err=%v, want known/nil", known, err)
		}
		if !reflect.DeepEqual(contexts, []string{"page-two-check"}) {
			t.Fatalf("contexts = %q, want the page-2 required check", contexts)
		}
		if args := strings.Join(runner.calls[1], " "); !strings.Contains(args, "--paginate") {
			t.Fatalf("rules call = %q, want every page requested", args)
		}
	})

	t.Run("protection without required checks is known and empty", func(t *testing.T) {
		runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"enforce_admins":{"enabled":false}}`}, {Stdout: "[]"}}}
		client := GhClient{Runner: runner}
		contexts, known, err := client.RequiredStatusCheckContexts(context.Background(), repo, "main")
		if err != nil || !known || len(contexts) != 0 {
			t.Fatalf("contexts=%q known=%v err=%v, want empty/known/nil", contexts, known, err)
		}
	})

	// The gate ignores an action_required check only on known=true, so every
	// read that is not a clean answer MUST come back undetermined.
	t.Run("undetermined answers never read as known", func(t *testing.T) {
		cases := []struct {
			name    string
			results []subprocess.Result
			errs    []error
		}{
			{"classic 404 unprotected branch", []subprocess.Result{{Stderr: "HTTP 404: Branch not protected"}}, []error{errors.New("exit status 1")}},
			{"classic permission failure with stale stdout", []subprocess.Result{{Stdout: classic, Stderr: "HTTP 403: Resource not accessible by integration"}}, []error{errors.New("exit status 1")}},
			{"classic undecodable", []subprocess.Result{{Stdout: ""}}, nil},
			{"rulesets failure", []subprocess.Result{{Stdout: classic}, {Stdout: rules, Stderr: "HTTP 502: Bad Gateway"}}, []error{nil, errors.New("exit status 1")}},
			{"rulesets undecodable", []subprocess.Result{{Stdout: classic}, {Stdout: "{}"}}, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				runner := &fakeRunner{results: tc.results, errs: tc.errs}
				client := GhClient{Runner: runner, MaxRetries: 0}
				contexts, known, err := client.RequiredStatusCheckContexts(context.Background(), repo, "main")
				if err != nil {
					t.Fatalf("returned error: %v", err)
				}
				if known || len(contexts) != 0 {
					t.Fatalf("contexts=%q known=%v for %q, want undetermined so the gate fails closed", contexts, known, tc.name)
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
				_, known, err := client.RequiredStatusCheckContexts(context.Background(), tc.repo, tc.branch)
				if err != nil || known {
					t.Fatalf("known=%v err=%v, want undetermined", known, err)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("calls = %v, want no gh invocation", runner.calls)
				}
			})
		}
	})
}
