package db

import (
	"strings"
	"testing"
)

// ListTasksByRepoState runs once per enabled repo on every one-second worker
// tick (the blocked-since evaluator), so it must SEARCH by repo and state rather
// than walk every task in id order (#2530). The only index leading with
// repo_full_name and state is partial on literal states, which a bound state
// cannot use. As in #2138 this asserts the plan of the shipped text, not a
// duration.
func TestListTasksByRepoStateSearchesRepoStateIndex(t *testing.T) {
	store := openWorkflowTestStore(t)
	plan := explainPlan(t, store, listTasksByRepoStateSQL, "owner/repo", "blocked")
	if !strings.Contains(plan, "SEARCH tasks USING INDEX idx_tasks_repo_state (repo_full_name=? AND state=?)") {
		t.Fatalf("ListTasksByRepoState does not search by repo and state:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("ListTasksByRepoState sorts its result instead of reading it in index order:\n%s", plan)
	}
}
