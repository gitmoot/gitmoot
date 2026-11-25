package github

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// #2755. The merge gate reads the complete base...head range when the pull
// request commit list (capped at 250) cannot vouch for completeness: every
// page of the paginated compare is read, and the result must hold exactly the
// range's total_commits entries.
func TestListCompareCommitsPaginatesTheCompleteRange(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{Stdout: `{"total":3,"shas":["c1","c2"]}
{"total":3,"shas":["c3"]}
`}}}
	client := GhClient{Runner: runner}

	commits, err := client.ListCompareCommits(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "base1", "head1")
	if err != nil {
		t.Fatalf("ListCompareCommits: %v", err)
	}
	if len(commits) != 3 || commits[0].SHA != "c1" || commits[2].SHA != "c3" {
		t.Fatalf("commits = %+v", commits)
	}
	runner.wantArgs(t, 0,
		"api", "--paginate", "--jq", "{total: .total_commits, shas: [.commits[].sha]}",
		"repos/gitmoot/gitmoot/compare/base1...head1?per_page=100",
	)
}

func TestListCompareCommitsRefusesAShortRange(t *testing.T) {
	for name, stdout := range map[string]string{
		"short":            `{"total":300,"shas":["c1","c2"]}`,
		"inconsistent":     "{\"total\":2,\"shas\":[\"c1\"]}\n{\"total\":3,\"shas\":[\"c2\"]}",
		"no total":         `{"shas":["c1"]}`,
		"empty output":     ``,
		"not a page shape": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			client := GhClient{Runner: &fakeRunner{results: []subprocess.Result{{Stdout: stdout}}}}
			commits, err := client.ListCompareCommits(context.Background(), Repository{Owner: "gitmoot", Name: "gitmoot"}, "base1", "head1")
			if err == nil {
				t.Fatalf("commits = %+v, want an error for an incomplete range", commits)
			}
			if name == "short" && !strings.Contains(err.Error(), "listed 2 of 300 commits") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
