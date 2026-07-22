package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestOrgPolicyResolverFailsClosed(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"bad/extra/path\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := orgPolicyResolver(home)("owner/repo"); got.LoadErr == nil {
		t.Fatal("malformed config did not surface LoadErr")
	}
}

func TestOrgCommandAndAgentOrgRolePrecedence(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runOrg([]string{"validate", "--home", home}, &out, &errOut); code != 0 || out.String() != "ok 1 roles\n" {
		t.Fatalf("validate code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := runOrg([]string{"show", "--home", home}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "owner\tparent=\tscope=*\tmerge_rule=owner") {
		t.Fatalf("show code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	t.Setenv("GITMOOT_ORG_ROLE", "env-role")
	options, ok := parseAgentAskOptions([]string{"agent", "message"}, &errOut)
	if !ok || options.orgRole != "env-role" {
		t.Fatalf("env options=%+v ok=%v", options, ok)
	}
	options, ok = parseAgentAskOptions([]string{"--org-role", "flag-role", "agent", "message"}, &errOut)
	if !ok || options.orgRole != "flag-role" {
		t.Fatalf("flag options=%+v ok=%v", options, ok)
	}
}

func TestOrgEscalateWritesTypedWorkflowNote(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{ID: "escalate-job", Agent: "agent", Type: "ask", State: "queued", Payload: `{"workflow_id":"release/one"}`}, "queued")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := runOrg([]string{"escalate", "--home", home, "--org-role", "OPERATOR", "--to", "OWNER", "--workflow", "release/one", "--repo", "acme/widget", "--json", "Can we include ] and x=y?"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("escalate code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if got := out.String(); !strings.Contains(got, `"from":"operator"`) || !strings.Contains(got, `"to":"owner"`) || !strings.Contains(got, `"workflow":"release/one"`) || !strings.Contains(got, `"question":"Can we include ] and x=y?"`) {
		t.Fatalf("escalate JSON = %q", got)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	notes, err := store.ListWorkflowNotes(context.Background(), "release/one", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %+v", notes)
	}
	want := db.WorkflowNote{WorkflowID: "release/one", Author: "operator", Body: "[org:escalate to=owner from=operator wf=release/one] Can we include ] and x=y?", Repo: "acme/widget"}
	if notes[0].WorkflowID != want.WorkflowID || notes[0].Author != want.Author || notes[0].Body != want.Body || notes[0].Repo != want.Repo {
		t.Fatalf("note = %+v, want fields %+v", notes[0], want)
	}
}

func TestOrgEscalateValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "same role", args: []string{"--org-role", "operator", "--to", "operator", "--workflow", "release/one", "question"}, want: "must be an ancestor"},
		{name: "unknown target", args: []string{"--org-role", "operator", "--to", "missing", "--workflow", "release/one", "question"}, want: `unknown org role "missing"`},
		{name: "sibling", args: []string{"--org-role", "operator", "--to", "auditor", "--workflow", "release/one", "question"}, want: "must be an ancestor"},
		{name: "unknown source", args: []string{"--org-role", "missing", "--to", "owner", "--workflow", "release/one", "question"}, want: `unknown org role "missing"`},
		{name: "missing workflow", args: []string{"--org-role", "operator", "--to", "owner", "question"}, want: "requires --workflow"},
		{name: "invalid workflow", args: []string{"--org-role", "operator", "--to", "owner", "--workflow", "Bad Label", "question"}, want: "invalid workflow id"},
		{name: "missing question", args: []string{"--org-role", "operator", "--to", "owner", "--workflow", "release/one"}, want: "requires exactly one question"},
		{name: "workflow has no jobs", args: []string{"--org-role", "operator", "--to", "owner", "--workflow", "release/one", "question"}, want: "has no jobs; refusing note to guard against a typo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := writeOrgEscalateConfig(t)
			args := append([]string{"escalate", "--home", home}, tt.args...)
			var out, errOut bytes.Buffer
			wantCode := 2
			if tt.name == "workflow has no jobs" {
				wantCode = 1
			}
			if code := runOrg(args, &out, &errOut); code != wantCode || !strings.Contains(errOut.String(), tt.want) {
				t.Fatalf("code=%d out=%q err=%q, want %q", code, out.String(), errOut.String(), tt.want)
			}
		})
	}
}

func TestOrgEscalateRegistryAndRolePrecedence(t *testing.T) {
	t.Run("registry required", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := runOrg([]string{"escalate", "--home", t.TempDir(), "--org-role", "operator", "--to", "owner", "--workflow", "release/one", "question"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "requires an [org] registry") {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	})
	t.Run("flag wins over environment", func(t *testing.T) {
		home := writeOrgEscalateConfig(t)
		store := openCLIJobStore(t, home)
		seedCLIJob(t, store, db.Job{ID: "precedence-job", Agent: "agent", Type: "ask", State: "queued", Payload: `{"workflow_id":"release/one"}`}, "queued")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITMOOT_ORG_ROLE", "auditor")
		var out, errOut bytes.Buffer
		if code := runOrg([]string{"escalate", "--home", home, "--org-role", "operator", "--to", "owner", "--workflow", "release/one", "question"}, &out, &errOut); code != 0 {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
		store = openCLIJobStore(t, home)
		defer store.Close()
		notes, err := store.ListWorkflowNotes(context.Background(), "release/one", 0)
		if err != nil || len(notes) != 1 || notes[0].Author != "operator" {
			t.Fatalf("notes=%+v err=%v", notes, err)
		}
	})
	t.Run("environment fallback", func(t *testing.T) {
		home := writeOrgEscalateConfig(t)
		store := openCLIJobStore(t, home)
		seedCLIJob(t, store, db.Job{ID: "fallback-job", Agent: "agent", Type: "ask", State: "queued", Payload: `{"workflow_id":"release/one"}`}, "queued")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITMOOT_ORG_ROLE", "operator")
		var out, errOut bytes.Buffer
		if code := runOrg([]string{"escalate", "--home", home, "--to", "owner", "--workflow", "release/one", "question"}, &out, &errOut); code != 0 {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	})
	t.Run("hyphen-leading question", func(t *testing.T) {
		home := writeOrgEscalateConfig(t)
		store := openCLIJobStore(t, home)
		seedCLIJob(t, store, db.Job{ID: "hyphen-job", Agent: "agent", Type: "ask", State: "queued", Payload: `{"workflow_id":"release/one"}`}, "queued")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := runOrg([]string{"escalate", "--home", home, "--org-role", "operator", "--to", "owner", "--workflow", "release/one", "-1 day left"}, &out, &errOut); code != 0 {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	})
}

func writeOrgEscalateConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "[org.roles.\"owner\"]\nscope=[\"*\"]\n[org.roles.\"maintainer\"]\nparent=\"owner\"\nscope=[\"*\"]\n[org.roles.\"operator\"]\nparent=\"maintainer\"\nscope=[\"*\"]\n[org.roles.\"auditor\"]\nparent=\"owner\"\nscope=[\"*\"]\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestOrgPreflightEnqueueParity(t *testing.T) {
	for _, tt := range []struct {
		name          string
		enforce       string
		role          string
		repo          string
		wantViolation bool
		wantErr       bool
	}{
		{name: "block role-less", enforce: "block", repo: "owner/repo", wantErr: true},
		{name: "block unknown", enforce: "block", role: "unknown", repo: "owner/repo", wantErr: true},
		{name: "block out-of-scope", enforce: "block", role: "owner", repo: "other/repo", wantErr: true},
		{name: "block in-scope", enforce: "block", role: "OWNER", repo: "owner/repo"},
		{name: "warn role-less", enforce: "warn", repo: "owner/repo", wantViolation: true},
		{name: "warn unknown", enforce: "warn", role: "unknown", repo: "owner/repo", wantViolation: true},
		{name: "warn out-of-scope", enforce: "warn", role: "owner", repo: "other/repo", wantViolation: true},
		{name: "warn in-scope", enforce: "warn", role: "owner", repo: "owner/repo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := orgParityPolicy(tt.enforce)
			violation, decisionErr := workflow.OrgScopeDecision(policy, tt.role, tt.repo)
			if (decisionErr != nil) != tt.wantErr || (violation != "") != tt.wantViolation {
				t.Fatalf("shared decision violation=%q err=%v", violation, decisionErr)
			}
			if err := preflightOrgScope(policy, tt.repo, tt.role, true); (err != nil) != tt.wantErr {
				t.Fatalf("preflight err=%v, shared err=%v", err, decisionErr)
			}
			store := openCLIJobStore(t, t.TempDir())
			defer store.Close()
			job, err := (workflow.Mailbox{Store: store, OrgPolicy: fixedOrgPolicy(policy)}).Enqueue(context.Background(), workflow.JobRequest{ID: "parity", Agent: "agent", Action: "ask", Repo: tt.repo, OperatorOrigin: true, ActingOrgRole: tt.role})
			if (err != nil) != tt.wantErr {
				t.Fatalf("enqueue err=%v, shared err=%v", err, decisionErr)
			}
			if err != nil {
				return
			}
			events, err := store.ListJobEvents(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotViolation := false
			for _, event := range events {
				gotViolation = gotViolation || event.Kind == "org_scope_violation"
			}
			if gotViolation != tt.wantViolation {
				t.Fatalf("warning event=%v want %v; events=%+v", gotViolation, tt.wantViolation, events)
			}
		})
	}
}

func orgParityPolicy(enforce string) workflow.OrgEnforcement {
	return workflow.OrgEnforcement{
		Enabled: true,
		Enforce: enforce,
		Role: func(name string) (workflow.OrgRole, bool) {
			if name == "owner" {
				return workflow.OrgRole{Name: "owner", Scope: []string{"owner/*"}}, true
			}
			return workflow.OrgRole{}, false
		},
		ScopeMatches: func(scopes []string, repo string) bool {
			return len(scopes) == 1 && scopes[0] == "owner/*" && strings.HasPrefix(repo, "owner/")
		},
	}
}
