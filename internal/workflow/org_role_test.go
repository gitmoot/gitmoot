package workflow

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestDelegationRequestInheritsActingOrgRole(t *testing.T) {
	request := (Engine{}).delegationRequest(
		db.Job{ID: "root", Agent: "coordinator"},
		JobPayload{Repo: "owner/repo", ActingOrgRole: "owner"},
		Delegation{ID: "review", Agent: "reviewer", Action: "review", Prompt: "review it"},
	)
	if request.ActingOrgRole != "owner" {
		t.Fatalf("ActingOrgRole = %q, want owner", request.ActingOrgRole)
	}
}
