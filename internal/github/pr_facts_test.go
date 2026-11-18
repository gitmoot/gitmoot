package github

import (
	"reflect"
	"testing"
)

// TestDecodeOpenPullRequestFactsRequiredChecks covers #2679 review P2-5: the
// base branch's required checks come from the same GraphQL response.
func TestDecodeOpenPullRequestFactsRequiredChecks(t *testing.T) {
	body := `{"data":{"rateLimit":{"cost":2},"repository":{"pullRequests":{"totalCount":3,"nodes":[
{"number":1,"headRefOid":"A","baseRef":{"branchProtectionRule":{"requiredStatusCheckContexts":["ci"],"requiredStatusChecks":[{"context":"ci"},{"context":"lint"}]},
 "rules":{"pageInfo":{"hasNextPage":false},"nodes":[{"type":"REQUIRED_STATUS_CHECKS","parameters":{"requiredStatusChecks":[{"context":"e2e"}]}},{"type":"DELETION","parameters":null}]}},"commits":{"nodes":[]}},
{"number":2,"headRefOid":"B","baseRef":{"branchProtectionRule":null,"rules":{"pageInfo":{"hasNextPage":false},"nodes":[{"type":"REQUIRED_STATUS_CHECKS","parameters":{"requiredStatusChecks":[{"context":"e2e"}]}}]}},"commits":{"nodes":[]}},
{"number":3,"headRefOid":"C","baseRef":{"branchProtectionRule":{"requiredStatusCheckContexts":[],"requiredStatusChecks":[]},"rules":{"pageInfo":{"hasNextPage":true},"nodes":[]}},"commits":{"nodes":[]}},
{"number":4,"headRefOid":"D","baseRef":{"branchProtectionRule":{"requiredStatusCheckContexts":["ci"],"requiredStatusChecks":[]},"rules":null},"commits":{"nodes":[]}}
]}}}}`
	facts, err := decodeOpenPullRequestFacts([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	type required struct {
		contexts []string
		known    bool
	}
	want := []required{
		{[]string{"ci", "lint", "e2e"}, true},
		{[]string{"e2e"}, false}, // classic protection unreadable or absent
		{nil, false},             // more ruleset rules than one page
		{[]string{"ci"}, false},  // rulesets not read (rules: null)
	}
	for i, pr := range facts.PullRequests {
		if got := (required{pr.Required, pr.RequiredKnown}); !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("PR %d required = %+v, want %+v", pr.Number, got, want[i])
		}
	}
}
