package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedConfiguredRole writes the org REGISTRY, which is what "the role exists"
// means. F2 of the #1920 review: the first version validated against the presence
// table instead, which records roles that have previously ACTED - so a newly
// configured role was refused until an unrelated command created its row, and a
// role deleted from the registry stayed acceptable forever.
func seedConfiguredRole(t *testing.T, home string, roles ...string) {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The registry requires its ROOT role to be named "owner", so the roles under
	// test are children of it. Learned from the loader's own refusal rather than
	// assumed: `root org role must be named "owner"`.
	body := "[org.roles.\"owner\"]\nscope=[\"*\"]\n"
	for _, role := range roles {
		if role == "owner" {
			continue
		}
		body += "[org.roles.\"" + role + "\"]\nparent=\"owner\"\nscope=[\"*\"]\n"
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// seedRolePresenceOnly creates the historical presence row WITHOUT configuring the
// role, which is the stale case F2 names: acceptable indefinitely under the old
// check, and it must now be refused.
func seedRolePresenceOnly(t *testing.T, home, role string) {
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
	seedConfiguredRole(t, home, "gitmoot", "lead-role")

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
			seedConfiguredRole(t, home, "gitmoot", "lead-role")

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

// TestJobRecordRoleValidationUsesTheConfiguredRegistry is F2 from the #1920 review,
// and it pins BOTH directions the presence table got wrong.
func TestJobRecordRoleValidationUsesTheConfiguredRegistry(t *testing.T) {
	t.Run("configured but never seen is ACCEPTED", func(t *testing.T) {
		home := t.TempDir()
		store := openCLIJobStore(t, home)
		seedSessionAgentRepo(t, store)
		store.Close()
		// registry only: deliberately NO presence row, which the old check required
		seedConfiguredRole(t, home, "fresh-role")

		var stdout, stderr bytes.Buffer
		if code := Run([]string{
			"job", "record", "--home", home, "--acting-role", "fresh-role",
			"--repo", "owner/repo", "--type", "implement", "--decision", "implemented", "--json",
		}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit = %d, want 0: a configured role must not wait for a presence row (stderr=%s)", code, stderr.String())
		}
	})

	t.Run("present but NOT configured is REFUSED", func(t *testing.T) {
		home := t.TempDir()
		store := openCLIJobStore(t, home)
		seedSessionAgentRepo(t, store)
		store.Close()
		seedConfiguredRole(t, home, "some-other-role")
		seedRolePresenceOnly(t, home, "removed-role")

		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"job", "record", "--home", home, "--acting-role", "removed-role",
			"--repo", "owner/repo", "--type", "implement", "--decision", "implemented",
		}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("a role with only a historical presence row was accepted; deletion from the registry must take effect")
		}
		if !strings.Contains(stderr.String(), `org role "removed-role" not found`) {
			t.Errorf("stderr = %q, want the unknown-role refusal", stderr.String())
		}
	})

	t.Run("no registry at all names the remedy", func(t *testing.T) {
		home := t.TempDir()
		store := openCLIJobStore(t, home)
		seedSessionAgentRepo(t, store)
		store.Close()

		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"job", "record", "--home", home, "--acting-role", "any-role",
			"--repo", "owner/repo", "--type", "implement", "--decision", "implemented",
		}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("a role was accepted with no organization registry configured")
		}
		if !strings.Contains(stderr.String(), "gitmoot org init") {
			t.Errorf("stderr = %q, want the refusal to name the remedy", stderr.String())
		}
	})
}
