package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedActingRole registers an org role the way every local ingress does, through
// the presence table that already tracks which roles have acted. No new registry
// is introduced by #1718: the role namespace already exists, it was simply never
// readable from the attribution path.
func seedActingRole(t *testing.T, home, role string) {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.TouchOrgRolePresence(context.Background(), role, "job record test"); err != nil {
		t.Fatalf("TouchOrgRolePresence returned error: %v", err)
	}
}

// TestJobRecordAcceptsAnActingRole is the #1916 reproduction, inverted into a
// regression. Before #1718 the ONLY way to record attribution was --agent, and the
// gate's own printed remedy therefore could not be run for work a coordinator did
// in session: `job record --agent gitmoot` is refused because gitmoot is an org
// role, not one of the registered agents. The honest path was closed, so the gate
// stayed blocked forever.
func TestJobRecordAcceptsAnActingRole(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedSessionAgentRepo(t, store)
	store.Close()
	seedActingRole(t, home, "gitmoot")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"job", "record", "--home", home,
		"--acting-role", "gitmoot",
		"--repo", "owner/repo",
		"--type", "implement",
		"--decision", "implemented",
		"--task", "task-9",
		"--pr", "1916",
		"--head-sha", "b34c928ea5c10dcc2d2a7575e6f290c4c5a3dfc9",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record --acting-role exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode session output: %v (stdout=%s)", err, stdout.String())
	}

	// THE ROW SHAPE IS THE CONTRACT BETWEEN THIS TEST AND THE GATE. The workflow
	// side (TestRoleImplementedTaskIsAttributable) asserts that a row with an EMPTY
	// agent column and this payload field satisfies attribution; this asserts the
	// CLI produces exactly that. The two meet at the row, so the same three fields
	// are pinned on both sides deliberately rather than by coincidence.
	verify := openCLIJobStore(t, home)
	defer verify.Close()
	job, err := verify.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q) returned error: %v", out.JobID, err)
	}
	if strings.TrimSpace(job.Agent) != "" {
		t.Errorf("recorded agent = %q, want EMPTY: naming an agent for role work is the false attribution this closes", job.Agent)
	}
	if job.Type != "implement" {
		t.Errorf("recorded type = %q, want implement", job.Type)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.ActingOrgRole != "gitmoot" {
		t.Errorf("recorded acting_org_role = %q, want gitmoot; without it the row is unattributable and the gate stays blocked", payload.ActingOrgRole)
	}
	if payload.TaskID != "task-9" || payload.PullRequest != 1916 {
		t.Errorf("recorded task/pr = %q/%d, want task-9/1916: attribution is matched by task identity", payload.TaskID, payload.PullRequest)
	}
}

// TestJobRecordActorFlagsAreExclusiveAndValidated pins the refusals. An escape
// hatch that accepts anything is worse than no escape hatch: it would let a caller
// record a role's work under an agent's name, or invent a role.
func TestJobRecordActorFlagsAreExclusiveAndValidated(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantExit int
		wantErr  string
	}{
		{
			name:     "an org role is still refused as an agent",
			args:     []string{"--agent", "gitmoot"},
			wantExit: 1,
			wantErr:  `agent "gitmoot" not found`,
		},
		{
			name:     "both actors at once",
			args:     []string{"--agent", "lead", "--acting-role", "gitmoot"},
			wantExit: 2,
			wantErr:  "not both",
		},
		{
			name:     "no actor at all",
			args:     []string{},
			wantExit: 2,
			wantErr:  "requires --agent or --acting-role",
		},
		{
			name:     "an unregistered role",
			args:     []string{"--acting-role", "not-a-real-role"},
			wantExit: 1,
			wantErr:  `org role "not-a-real-role" not found`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			seedSessionAgentRepo(t, store)
			store.Close()
			seedActingRole(t, home, "gitmoot")

			args := append([]string{
				"job", "record", "--home", home,
				"--repo", "owner/repo", "--type", "implement", "--decision", "implemented",
			}, tc.args...)
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stderr=%s)", code, tc.wantExit, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.wantErr)
			}
		})
	}
}
