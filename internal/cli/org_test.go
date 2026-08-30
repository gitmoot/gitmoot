package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "owner-route", OnKind: "reply", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		PaneBindings: map[string]org.PaneBinding{"owner": {PaneID: "w1:p1"}},
		Panes:        []org.LivePane{{PaneID: "w1:p1", Label: "owner"}},
	}})
	var out, errOut bytes.Buffer
	if code := runOrg([]string{"validate", "--home", home}, &out, &errOut); code != 0 || out.String() != "ok 1 roles, 1 live panes, 1 enabled routes\n" {
		t.Fatalf("validate code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	// This fixture intentionally omits merge_rule. Empty means no authority and
	// must not be rendered as the owner grant.
	out.Reset()
	if code := runOrg([]string{"show", "--home", home}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "owner\tparent=\tscope=*\tmerge_rule=none") {
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

func TestOrgValidateAssertsLiveBindingsRoutesAndPaneClaims(t *testing.T) {
	tests := []struct {
		name          string
		snapshot      org.Snapshot
		routeRoles    []string
		wantCode      int
		wantOutput    []string
		notWantOutput string
	}{
		{
			name: "healthy",
			snapshot: org.Snapshot{
				PaneBindings: map[string]org.PaneBinding{
					"owner": {PaneID: "w1:p1"}, "review": {PaneID: "w1:p2"},
				},
				Panes: []org.LivePane{{PaneID: "w1:p1", Label: "owner"}, {PaneID: "w1:p2", Label: "review"}},
			},
			routeRoles: []string{"owner", "review"},
			wantOutput: []string{"ok 2 roles, 2 live panes, 2 enabled routes"},
		},
		{
			name: "mutant unresolved role binding",
			snapshot: org.Snapshot{
				PaneBindings: map[string]org.PaneBinding{
					"owner": {PaneID: "w1:p1"}, "review": {Detail: `no Herdr pane bound as "missing"`},
				},
				Panes: []org.LivePane{{PaneID: "w1:p1", Label: "owner"}},
			},
			routeRoles: []string{"owner", "review"},
			wantCode:   1,
			wantOutput: []string{
				"unresolved_roles=1 roles_without_routes=0 unclaimed_labeled_panes=0",
				`role review pane unresolved: no Herdr pane bound as "missing"`,
				"paths=presence,event-wake,role-unavailable-nudge",
			},
			notWantOutput: "ok 2 roles",
		},
		{
			name: "mutant last route removed",
			snapshot: org.Snapshot{
				PaneBindings: map[string]org.PaneBinding{
					"owner": {PaneID: "w1:p1"}, "review": {PaneID: "w1:p2"},
				},
				Panes: []org.LivePane{{PaneID: "w1:p1", Label: "owner"}, {PaneID: "w1:p2", Label: "review"}},
			},
			routeRoles: []string{"owner"},
			wantCode:   1,
			wantOutput: []string{
				"unresolved_roles=0 roles_without_routes=1 unclaimed_labeled_panes=0",
				"role review has no enabled wake route",
			},
			notWantOutput: "ok 2 roles",
		},
		{
			name: "mutant unclaimed labeled pane",
			snapshot: org.Snapshot{
				PaneBindings: map[string]org.PaneBinding{
					"owner": {PaneID: "w1:p1"}, "review": {PaneID: "w1:p2"},
				},
				Panes: []org.LivePane{
					{PaneID: "w1:p1", Label: "owner"}, {PaneID: "w1:p2", Label: "review"},
					{PaneID: "w1:p3", Label: "orphan"},
				},
			},
			routeRoles: []string{"owner", "review"},
			wantCode:   1,
			wantOutput: []string{
				"unresolved_roles=0 roles_without_routes=0 unclaimed_labeled_panes=1",
				`labeled pane "orphan" (w1:p3) is not claimed by an org role`,
			},
			notWantOutput: "ok 2 roles",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope = ["*"]
pane = "w1:p1"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
pane = "w1:p2"
`), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			for index, role := range test.routeRoles {
				if err := store.AddEventRule(context.Background(), db.EventRule{
					ID: fmt.Sprintf("route-%d", index), OnKind: "reply", WakeRole: role, Enabled: true,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "mutant unresolved role binding" {
				if err := store.IncrementRoleMissedWake(context.Background(), "review", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			withOrgProvider(t, orgFixtureProvider{snapshot: test.snapshot})
			var stdout, stderr bytes.Buffer
			code := runOrg([]string{"validate", "--home", home}, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if test.wantCode != 0 {
				t.Logf("validate failure output:\n%s", output)
			}
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; output:\n%s", code, test.wantCode, output)
			}
			for _, want := range test.wantOutput {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
			if test.notWantOutput != "" && strings.Contains(output, test.notWantOutput) {
				t.Fatalf("output unexpectedly contains %q:\n%s", test.notWantOutput, output)
			}
		})
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
	outbox, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil || len(outbox) != 1 || outbox[0].State != db.WakeOutboxStatePending ||
		outbox[0].TargetRole != "owner" || outbox[0].SourceID != fmt.Sprint(notes[0].ID) {
		t.Fatalf("escalation wake outbox = %+v, err=%v", outbox, err)
	}
}

func TestOrgEscalateDescendantWritesAddressedQuestion(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID: "downward-ask-job", Agent: "agent", Type: "ask", State: "queued",
		Payload: `{"workflow_id":"release/downward"}`,
	}, "queued")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"escalate", "--home", home, "--org-role", "owner", "--to", "operator",
		"--workflow", "release/downward", "--repo", "acme/widget", "Can you verify the release?",
	}, &stdout, &stderr)

	store = openCLIJobStore(t, home)
	defer store.Close()
	ctx := context.Background()
	notes, notesErr := store.ListWorkflowNotes(ctx, "release/downward", 0)
	outbox, outboxErr := store.ListWakeOutbox(ctx, "")
	if notesErr != nil || outboxErr != nil || len(notes) != 1 || len(outbox) != 1 {
		t.Fatalf(
			"stored descendant ask: notes=%+v notes_err=%v wake_outbox=%+v outbox_err=%v; run code=%d out=%q err=%q",
			notes, notesErr, outbox, outboxErr, code, stdout.String(), stderr.String(),
		)
	}
	wantBody := workflow.FormatOrgEscalateNote(
		"owner", "operator", "release/downward", "Can you verify the release?",
	)
	if notes[0].Author != "owner" || notes[0].Body != wantBody || notes[0].Repo != "acme/widget" {
		t.Fatalf("stored descendant note = %+v, want author=owner body=%q repo=acme/widget", notes[0], wantBody)
	}
	if outbox[0].SourceKind != db.WakeOutboxSourceWorkflowNote ||
		outbox[0].SourceID != fmt.Sprint(notes[0].ID) ||
		outbox[0].TargetRole != "operator" ||
		outbox[0].State != db.WakeOutboxStatePending {
		t.Fatalf(
			"stored descendant wake outbox = %+v, want pending workflow-note row source=%d addressed to operator",
			outbox, notes[0].ID,
		)
	}
	if code != 0 || stdout.String() != "asked from owner to operator in workflow release/downward\n" || stderr.Len() != 0 {
		t.Fatalf("descendant ask code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgEscalateAncestorBehaviorRemainsEscalation(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID: "upward-escalation-job", Agent: "agent", Type: "ask", State: "queued",
		Payload: `{"workflow_id":"release/upward"}`,
	}, "queued")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"escalate", "--home", home, "--org-role", "operator", "--to", "owner",
		"--workflow", "release/upward", "May we ship?",
	}, &stdout, &stderr)

	store = openCLIJobStore(t, home)
	defer store.Close()
	notes, err := store.ListWorkflowNotes(context.Background(), "release/upward", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("stored ancestor escalation notes=%+v err=%v; run code=%d out=%q err=%q", notes, err, code, stdout.String(), stderr.String())
	}
	wantBody := workflow.FormatOrgEscalateNote("operator", "owner", "release/upward", "May we ship?")
	if notes[0].Author != "operator" || notes[0].Body != wantBody {
		t.Fatalf("stored ancestor escalation = %+v, want author=operator body=%q", notes[0], wantBody)
	}
	if code != 0 || stdout.String() != "escalated from operator to owner in workflow release/upward\n" || stderr.Len() != 0 {
		t.Fatalf("ancestor escalation code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgEscalateResolveLifecycle(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	target, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one",
		Author:     "operator",
		Body:       workflow.FormatOrgEscalateNote("operator", "owner", "release/one", "Need a decision."),
		Repo:       "acme/widget",
	})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "owner", Body: "Approved for release.", Repo: "acme/widget",
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/one", Author: "owner", Body: "ordinary note",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runOrg([]string{
		"escalate", "resolve", fmt.Sprint(target.ID),
		"--home", home, "--note", fmt.Sprint(answer.ID),
	}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), fmt.Sprintf("resolved escalation %d in workflow release/one", target.ID)) {
		t.Fatalf("resolve code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	store = openCLIJobStore(t, home)
	resolutions, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgEscalateResolvedPrefix, 0)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(resolutions) != 1 {
		store.Close()
		t.Fatalf("resolution notes = %+v", resolutions)
	}
	escalationNoteID, resolvedBy, answerNoteID, ok := workflow.ParseOrgEscalateResolvedNote(resolutions[0].Body)
	if !ok || escalationNoteID != target.ID || resolvedBy != "owner" || answerNoteID != answer.ID {
		store.Close()
		t.Fatalf("resolution marker = (%d, %q, %d, %v), body=%q", escalationNoteID, resolvedBy, answerNoteID, ok, resolutions[0].Body)
	}
	if resolutions[0].WorkflowID != target.WorkflowID || resolutions[0].Author != "owner" || resolutions[0].Repo != target.Repo {
		store.Close()
		t.Fatalf("resolution note = %+v, target = %+v", resolutions[0], target)
	}
	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(outbox) != 1 ||
		outbox[0].SourceKind != db.WakeOutboxSourceWorkflowNote ||
		outbox[0].SourceID != fmt.Sprint(resolutions[0].ID) ||
		outbox[0].TargetRole != "operator" ||
		outbox[0].State != db.WakeOutboxStatePending {
		store.Close()
		t.Fatalf("resolution wake outbox = %+v, want stored pending workflow-note row source=%d addressed to operator", outbox, resolutions[0].ID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		id   int64
		want string
	}{
		{name: "non escalation", id: ordinary.ID, want: fmt.Sprintf("note %d is not an org escalation", ordinary.ID)},
		{name: "missing", id: resolutions[0].ID + 1000, want: "not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			out.Reset()
			errOut.Reset()
			code := runOrg([]string{"escalate", "resolve", fmt.Sprint(test.id), "--home", home}, &out, &errOut)
			if code != 1 || !strings.Contains(errOut.String(), test.want) {
				t.Fatalf("resolve code=%d out=%q err=%q, want %q", code, out.String(), errOut.String(), test.want)
			}
		})
	}
}

func TestOrgEscalateResolveWithoutIdentifiableAskerRecordsObservableUnaddressedResolution(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	target, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/legacy",
		Author:     "legacy",
		Body:       "[org:escalate to=owner wf=release/legacy] Need a decision.",
		Repo:       "acme/widget",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"escalate", "resolve", fmt.Sprint(target.ID), "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolve code=%d out=%q err=%q, want resolution recorded", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no identifiable asker") || !strings.Contains(stderr.String(), "recorded without wake") {
		t.Fatalf("resolve stderr=%q, want observable unaddressed-resolution warning", stderr.String())
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	resolutions, err := store.ListWorkflowNotesByBodyPrefix(ctx, workflow.OrgEscalateResolvedPrefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolutions) != 1 {
		t.Fatalf("stored resolution notes = %+v, want one", resolutions)
	}
	outbox, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 0 {
		t.Fatalf("stored wake outbox = %+v, want no invented target", outbox)
	}
}

// TestOrgEscalateResolveIDPlacement guards orgEscalateResolveIDAndFlags: the
// escalation note id must resolve correctly regardless of where it appears
// among the flags (its whole reason for existing over flag.Args()).
func TestOrgEscalateResolveIDPlacement(t *testing.T) {
	home := writeOrgEscalateConfig(t)
	for _, test := range []struct {
		name string
		wf   string
		args func(home string, id int64) []string
	}{
		{name: "id first", wf: "release/id-first", args: func(home string, id int64) []string {
			return []string{"escalate", "resolve", fmt.Sprint(id), "--home", home, "--by", "owner"}
		}},
		{name: "id last", wf: "release/id-last", args: func(home string, id int64) []string {
			return []string{"escalate", "resolve", "--home", home, "--by", "owner", fmt.Sprint(id)}
		}},
		{name: "id between flags", wf: "release/id-between", args: func(home string, id int64) []string {
			return []string{"escalate", "resolve", "--home", home, fmt.Sprint(id), "--by", "owner"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openCLIJobStore(t, home)
			ctx := context.Background()
			target, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
				WorkflowID: test.wf, Author: "operator",
				Body: workflow.FormatOrgEscalateNote("operator", "owner", test.wf, "Need a decision."),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			code := runOrg(test.args(home, target.ID), &out, &errOut)
			if code != 0 || !strings.Contains(out.String(), fmt.Sprintf("resolved escalation %d", target.ID)) {
				t.Fatalf("resolve code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
		})
	}
}

// TestOrgEscalateResolveHelp guards the fix for --help being swallowed by the
// id-extraction scan before flag.Parse ever ran (the scan treated --help as a
// skippable flag, never set an id, and fell into the generic "requires exactly
// one escalation note id" error instead of printing usage).
func TestOrgEscalateResolveHelp(t *testing.T) {
	for _, args := range [][]string{
		{"escalate", "resolve", "--help"},
		{"escalate", "resolve", "-h"},
		{"escalate", "resolve", "--by", "owner", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runOrg(args, &out, &errOut)
			if code != 0 {
				t.Fatalf("resolve --help code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			if strings.Contains(errOut.String(), "requires exactly one escalation note id") {
				t.Fatalf("--help fell through to the generic id error instead of usage: err=%q", errOut.String())
			}
			if !strings.Contains(errOut.String(), "-by") && !strings.Contains(errOut.String(), "-note") {
				t.Fatalf("--help did not print flag usage: err=%q", errOut.String())
			}
		})
	}
}

func TestOrgEscalateValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "same role", args: []string{"--org-role", "operator", "--to", "operator", "--workflow", "release/one", "question"}, want: "must differ from acting role"},
		{name: "unknown target", args: []string{"--org-role", "operator", "--to", "missing", "--workflow", "release/one", "question"}, want: `unknown org role "missing"`},
		{name: "sibling", args: []string{"--org-role", "operator", "--to", "auditor", "--workflow", "release/one", "question"}, want: "peer questions are not configurable and are refused"},
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
	t.Parallel()

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
			mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("org enqueue test"))
			mailbox.OrgPolicy = fixedOrgPolicy(policy)
			job, err := mailbox.Enqueue(context.Background(), workflow.JobRequest{ID: "parity", Agent: "agent", Action: "ask", Repo: tt.repo, OperatorOrigin: true, ActingOrgRole: tt.role})
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

type orgFixtureProvider struct {
	snapshot org.Snapshot
	err      error
	recycle  func(context.Context, org.RecycleRequest) error
}

func (p orgFixtureProvider) Snapshot(context.Context) (org.Snapshot, error) {
	return p.snapshot, p.err
}

func (p orgFixtureProvider) Recycle(ctx context.Context, req org.RecycleRequest) error {
	if p.recycle == nil {
		return nil
	}
	return p.recycle(ctx, req)
}

type orgFixtureRunner struct {
	version string
}

func (r orgFixtureRunner) LookPath(string) (string, error) { return "/usr/bin/herdr", nil }

func (r orgFixtureRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{Stdout: r.version}, nil
}

func setupOrgHome(t *testing.T) (string, config.Paths) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
display_name = "Owner"
scope = ["*"]
merge_rule = "owner"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
merge_rule = "self"
`)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return home, paths
}

func withOrgProvider(t *testing.T, provider org.Provider) {
	t.Helper()
	original := newOrgProvider
	newOrgProvider = func([]config.OrgRole) org.Provider { return provider }
	t.Cleanup(func() { newOrgProvider = original })
}

func TestOrgProviderSnapshotPassesRolePaneBindings(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope = ["*"]
pane = "Gitmoot"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}

	original := newOrgProvider
	var got []config.OrgRole
	newOrgProvider = func(roles []config.OrgRole) org.Provider {
		got = append([]config.OrgRole(nil), roles...)
		return orgFixtureProvider{snapshot: org.Snapshot{ObservedAt: time.Now()}}
	}
	t.Cleanup(func() { newOrgProvider = original })

	if _, err := orgProviderSnapshot(context.Background(), cfg); err != nil {
		t.Fatalf("orgProviderSnapshot() error = %v", err)
	}
	if len(got) != 2 || got[0].Name != "owner" || got[0].Pane != "Gitmoot" || got[1].Name != "review" {
		t.Fatalf("provider roles = %+v", got)
	}
}

func TestRunOrgBriefChartStatusAndPresence(t *testing.T) {
	home, paths := setupOrgHome(t)
	observed := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateWorking},
			"review": {State: org.StateIdle},
		},
		ObservedAt: observed, ProviderVersion: "0.7.5",
	}})

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "brief", "--home", home, "--role", "review", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("brief code = %d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"pane"`) {
		t.Fatalf("paneless brief unexpectedly emitted pane: %s", stdout.String())
	}
	var brief orgBriefOutput
	if err := json.Unmarshal(stdout.Bytes(), &brief); err != nil {
		t.Fatalf("decode brief: %v; output=%s", err, stdout.String())
	}
	if brief.Role != "review" || brief.Parent != "owner" || brief.ProviderState != org.StateIdle || brief.LastSeenAt == "" || brief.LastCommand != "org brief" {
		t.Fatalf("brief = %+v", brief)
	}
	if got := strings.Join(brief.Path, "/"); got != "owner/review" {
		t.Fatalf("brief path = %q", got)
	}

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	presence, err := store.ListOrgRolePresence(context.Background())
	store.Close()
	if err != nil || len(presence) != 1 || presence[0].Role != "review" || presence[0].LastCommand != "org brief" {
		t.Fatalf("presence = %+v, err=%v", presence, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "chart", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("chart code = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "owner · working") || !strings.Contains(stdout.String(), "  review · idle") {
		t.Fatalf("chart output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "status", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status code = %d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"pane"`) {
		t.Fatalf("paneless status unexpectedly emitted pane: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), `"recycle"`) {
		t.Fatalf("status without recycle policy unexpectedly emitted recycle fields: %s", stdout.String())
	}
	var status []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || len(status) != 2 {
		t.Fatalf("status = %+v err=%v output=%s", status, err, stdout.String())
	}
}

func TestRunOrgBriefRoleResolution(t *testing.T) {
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{States: map[string]org.RoleLiveState{}}})
	tests := []struct {
		name       string
		envRole    string
		flagRole   string
		wantRole   string
		wantCode   int
		wantStderr string
	}{
		{name: "env_fallback", envRole: "review", wantRole: "review"},
		{name: "flag_precedence", envRole: "owner", flagRole: "review", wantRole: "review"},
		{name: "missing_role", wantCode: 2, wantStderr: "org brief requires --role NAME\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, paths := setupOrgHome(t)
			t.Setenv("GITMOOT_ORG_ROLE", test.envRole)
			args := []string{"org", "brief", "--home", home, "--json"}
			if test.flagRole != "" {
				args = append(args, "--role", test.flagRole)
			}
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != test.wantCode {
				t.Fatalf("brief code = %d, want %d; stderr=%q", code, test.wantCode, stderr.String())
			}
			if test.wantCode != 0 {
				if stderr.String() != test.wantStderr {
					t.Fatalf("brief stderr = %q, want %q", stderr.String(), test.wantStderr)
				}
				return
			}
			var brief orgBriefOutput
			if err := json.Unmarshal(stdout.Bytes(), &brief); err != nil {
				t.Fatalf("decode brief: %v; output=%s", err, stdout.String())
			}
			if brief.Role != test.wantRole {
				t.Fatalf("brief role = %q, want %q", brief.Role, test.wantRole)
			}
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			presence, err := store.ListOrgRolePresence(context.Background())
			_ = store.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(presence) != 1 || presence[0].Role != test.wantRole || presence[0].LastCommand != "org brief" {
				t.Fatalf("presence = %+v, want one %s org brief stamp", presence, test.wantRole)
			}
		})
	}
}

func TestRunOrgBriefAndChartSurfaceModelPin(t *testing.T) {
	home, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("model = \"sonnet\"\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{States: map[string]org.RoleLiveState{
		"owner": {State: org.StateWorking}, "review": {State: org.StateIdle},
	}}})

	for _, test := range []struct {
		role string
		want string
	}{
		{role: "review", want: "model: sonnet\n"},
		{role: "owner", want: "model: -\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"org", "brief", "--home", home, "--role", test.role}, &stdout, &stderr); code != 0 {
			t.Fatalf("brief %s code = %d stderr=%s", test.role, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("brief %s output = %q, want containing %q", test.role, stdout.String(), test.want)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "chart", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("chart code = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "owner · working · scope=* · merge=owner · model=-") ||
		!strings.Contains(stdout.String(), "review · idle · scope=gitmoot/* · merge=self · model=sonnet") {
		t.Fatalf("chart output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "brief", "--home", home, "--role", "review", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("brief --json code = %d stderr=%s", code, stderr.String())
	}
	var brief orgBriefOutput
	if err := json.Unmarshal(stdout.Bytes(), &brief); err != nil || brief.Model != "sonnet" {
		t.Fatalf("brief.Model = %+v err=%v output=%s", brief, err, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "status", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status --json code = %d stderr=%s", code, stderr.String())
	}
	var status []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v; output=%s", err, stdout.String())
	}
	seen := map[string]bool{}
	for _, row := range status {
		switch row.Role {
		case "review":
			if row.Model != "sonnet" {
				t.Fatalf("review status model = %q, want sonnet", row.Model)
			}
			seen["review"] = true
		case "owner":
			if row.Model != "" {
				t.Fatalf("owner status model = %q, want empty (unpinned)", row.Model)
			}
			seen["owner"] = true
		}
	}
	if !seen["review"] || !seen["owner"] {
		t.Fatalf("expected roles missing from status: %+v", status)
	}
}

func TestRunOrgOverviewFlagsConsecutiveMissedWakes(t *testing.T) {
	home, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[orchestrate]\nmax_consecutive_missed_wakes = 2\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 2 {
		if err := store.IncrementRoleMissedWake(ctx, "REVIEW", time.Now()); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateWorking},
			"review": {State: org.StateIdle},
		},
		ObservedAt: time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC), ProviderVersion: "0.7.5",
	}})

	for _, command := range []string{"chart", "status"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"org", command, "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s code = %d stderr=%s", command, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "review") || !strings.Contains(stdout.String(), "⚠ flagged (2 missed wakes)") {
			t.Fatalf("%s output = %q, want flagged review", command, stdout.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "status", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status JSON code = %d stderr=%s", code, stderr.String())
	}
	var rows []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode status JSON: %v; output=%s", err, stdout.String())
	}
	for _, row := range rows {
		if row.Role == "review" {
			if row.MissedWakes != 2 || !row.Flagged || row.FlagReason != "2 consecutive missed wakes" {
				t.Fatalf("review status = %+v", row)
			}
			return
		}
	}
	t.Fatalf("review role missing from status: %+v", rows)
}

func TestRunOrgOverviewZeroThresholdNeverFlags(t *testing.T) {
	home, paths := setupOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IncrementRoleMissedWake(context.Background(), "review", time.Now()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{States: map[string]org.RoleLiveState{
		"owner": {State: org.StateWorking}, "review": {State: org.StateIdle},
	}}})

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "chart", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("chart JSON code = %d stderr=%s", code, stderr.String())
	}
	// Off-by-default: with the threshold at 0, the counter is FULLY invisible — no
	// flag fields AND no missed_wakes leak — even though "review" has a stored miss.
	if strings.Contains(stdout.String(), `"flagged"`) || strings.Contains(stdout.String(), `"flag_reason"`) || strings.Contains(stdout.String(), `"missed_wakes"`) {
		t.Fatalf("zero threshold surfaced missed-wake state: %s", stdout.String())
	}
	var rows []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Role == "review" && (row.Flagged || row.FlagReason != "" || row.MissedWakes != 0) {
			t.Fatalf("review status with threshold off = %+v", row)
		}
	}
}

// TestRunOrgOverviewDegradesOnBadOrchestrateConfig pins that a malformed
// [orchestrate] value never breaks the org chart/status diagnostic view — the
// missed-wake flag is a best-effort add-on that degrades to "off".
func TestRunOrgOverviewDegradesOnBadOrchestrateConfig(t *testing.T) {
	home, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[orchestrate]\nmax_consecutive_missed_wakes = -1\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{States: map[string]org.RoleLiveState{
		"owner": {State: org.StateWorking}, "review": {State: org.StateIdle},
	}}})
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "chart", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("org chart hard-failed on a bad [orchestrate] value: code=%d stderr=%s", code, stderr.String())
	}
	var rows []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("org chart output not renderable after degrade: %v (out=%s)", err, stdout.String())
	}
	if len(rows) == 0 {
		t.Fatal("org chart returned no rows despite a valid [org] registry")
	}
}

func TestRunOrgBriefAndStatusJSONSurfacePane(t *testing.T) {
	home, paths := setupOrgHome(t)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("pane = \"w1:p2\"\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateWorking},
			"review": {State: org.StateIdle},
		},
		ObservedAt: time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC), ProviderVersion: "0.7.5",
	}})

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "brief", "--home", home, "--role", "review", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("brief code = %d stderr=%s", code, stderr.String())
	}
	var brief orgBriefOutput
	if err := json.Unmarshal(stdout.Bytes(), &brief); err != nil || brief.Pane != "w1:p2" {
		t.Fatalf("brief = %+v err=%v output=%s", brief, err, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "status", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status code = %d stderr=%s", code, stderr.String())
	}
	var status []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v; output=%s", err, stdout.String())
	}
	for _, row := range status {
		if row.Role == "review" {
			if row.Pane != "w1:p2" {
				t.Fatalf("review status pane = %q, want w1:p2", row.Pane)
			}
			return
		}
	}
	t.Fatalf("review role missing from status: %+v", status)
}

func TestOrgRecycleStatus(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		lastSeen     string
		state        org.LifecycleState
		activeJobs   int
		recycleAfter time.Duration
		want         string
	}{
		{name: "off without policy", lastSeen: now.Add(-48 * time.Hour).Format(time.RFC3339), state: org.StateIdle, want: "off"},
		{name: "off without presence", state: org.StateIdle, recycleAfter: 24 * time.Hour, want: "off"},
		{name: "off with malformed presence", lastSeen: "not-a-timestamp", state: org.StateIdle, recycleAfter: 24 * time.Hour, want: "off"},
		{name: "fresh", lastSeen: now.Add(-time.Hour).Format(time.RFC3339), state: org.StateWorking, recycleAfter: 24 * time.Hour, want: "fresh"},
		{name: "eligible idle", lastSeen: now.Add(-24 * time.Hour).Format(time.RFC3339), state: org.StateIdle, recycleAfter: 24 * time.Hour, want: "eligible"},
		{name: "eligible done", lastSeen: now.Add(-25 * time.Hour).Format(time.RFC3339), state: org.StateDone, recycleAfter: 24 * time.Hour, want: "eligible"},
		{name: "eligible unknown", lastSeen: now.Add(-25 * time.Hour).Format(time.RFC3339), state: org.StateUnknown, recycleAfter: 24 * time.Hour, want: "eligible"},
		{name: "overdue working", lastSeen: now.Add(-25 * time.Hour).Format(time.RFC3339), state: org.StateWorking, recycleAfter: 24 * time.Hour, want: "overdue"},
		{name: "overdue blocked", lastSeen: now.Add(-25 * time.Hour).Format(time.RFC3339), state: org.StateBlocked, recycleAfter: 24 * time.Hour, want: "overdue"},
		{name: "overdue active job", lastSeen: now.Add(-25 * time.Hour).Format(time.RFC3339), state: org.StateIdle, activeJobs: 1, recycleAfter: 24 * time.Hour, want: "overdue"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := orgRecycleStatus(test.lastSeen, now, test.state, test.activeJobs, test.recycleAfter); got != test.want {
				t.Fatalf("orgRecycleStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunOrgStatusRecycleStates(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
[org.roles."fresh"]
parent = "owner"
scope = ["*"]
recycle_after = "24h"
[org.roles."eligible"]
parent = "owner"
scope = ["*"]
recycle_after = "1ns"
[org.roles."working"]
parent = "owner"
scope = ["*"]
recycle_after = "1ns"
[org.roles."active"]
parent = "owner"
scope = ["*"]
recycle_after = "1ns"
`)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":    {State: org.StateUnknown},
			"fresh":    {State: org.StateIdle},
			"eligible": {State: org.StateDone},
			"working":  {State: org.StateWorking},
			"active":   {State: org.StateIdle},
		},
		ObservedAt: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC), ProviderVersion: "0.7.5",
	}})
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, role := range []string{"fresh", "eligible", "working", "active"} {
		if err := store.TouchOrgRolePresence(ctx, role, "test"); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	for _, job := range []db.Job{
		{ID: "owner-queued-job", Agent: "worker", Type: "ask", State: "queued", Payload: `{"acting_org_role":"owner"}`},
		{ID: "owner-running-job", Agent: "worker", Type: "ask", State: "running", Payload: `{"acting_org_role":"owner"}`},
		{ID: "active-job", Agent: "worker", Type: "ask", State: "queued", Payload: `{"acting_org_role":"active"}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "status", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status JSON code = %d stderr=%s", code, stderr.String())
	}
	var rows []orgStatusOutput
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode status: %v; output=%s", err, stdout.String())
	}
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &rawRows); err != nil {
		t.Fatalf("decode raw status: %v; output=%s", err, stdout.String())
	}
	rawByRole := make(map[string]map[string]json.RawMessage, len(rawRows))
	for _, raw := range rawRows {
		var role string
		if err := json.Unmarshal(raw["role"], &role); err != nil {
			t.Fatalf("decode raw status role: %v; row=%s", err, raw["role"])
		}
		rawByRole[role] = raw
	}
	want := map[string]struct {
		status, after string
		activeJobs    int
	}{
		"owner":    {activeJobs: 2},
		"fresh":    {status: "fresh", after: "24h"},
		"eligible": {status: "eligible", after: "1ns"},
		"working":  {status: "overdue", after: "1ns"},
		"active":   {status: "overdue", after: "1ns", activeJobs: 1},
	}
	for _, row := range rows {
		expected, ok := want[row.Role]
		if !ok {
			t.Fatalf("unexpected status role %+v", row)
		}
		if row.RecycleStatus != expected.status || row.RecycleAfter != expected.after {
			t.Fatalf("status[%s] recycle=%q after=%q, want %q/%q", row.Role, row.RecycleStatus, row.RecycleAfter, expected.status, expected.after)
		}
		if row.ActiveJobs != expected.activeJobs {
			t.Fatalf("status[%s] active_jobs=%d, want %d", row.Role, row.ActiveJobs, expected.activeJobs)
		}
		_, hasActiveJobs := rawByRole[row.Role]["active_jobs"]
		if hasActiveJobs != (expected.activeJobs > 0) {
			t.Fatalf("status[%s] active_jobs JSON presence=%v, want %v", row.Role, hasActiveJobs, expected.activeJobs > 0)
		}
		delete(want, row.Role)
	}
	if len(want) != 0 {
		t.Fatalf("missing status roles: %+v", want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"org", "status", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("status text code = %d stderr=%s", code, stderr.String())
	}
	for role, expected := range map[string]string{"owner": "off", "fresh": "fresh", "eligible": "eligible", "working": "overdue", "active": "overdue"} {
		found := false
		for _, line := range strings.Split(stdout.String(), "\n") {
			if strings.HasPrefix(line, role+"\t") && strings.Contains(line, "recycle="+expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("status text missing %s recycle=%s: %q", role, expected, stdout.String())
		}
	}
}

func TestRunOrgProviderFailureSemantics(t *testing.T) {
	home, _ := setupOrgHome(t)
	withOrgProvider(t, orgFixtureProvider{err: errors.New("socket unavailable")})
	for _, command := range []string{"chart", "status"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"org", command, "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "snapshot unavailable") {
			t.Fatalf("%s code/stderr = %d/%q", command, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "brief", "--home", home, "--role", "owner", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("brief code = %d stderr=%s", code, stderr.String())
	}
	var brief orgBriefOutput
	if err := json.Unmarshal(stdout.Bytes(), &brief); err != nil {
		t.Fatal(err)
	}
	if brief.ProviderState != org.StateUnknown || !strings.Contains(brief.ProviderDetail, "socket unavailable") {
		t.Fatalf("brief = %+v", brief)
	}
}

func TestRunOrgRecycleValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		pane string
		args []string
		want string
	}{
		{name: "missing handoff", pane: "w1:p2", args: []string{"owner", "--kind", "codex"}, want: "requires a non-empty --handoff"},
		{name: "missing kind", pane: "w1:p2", args: []string{"owner", "--handoff", "done"}, want: "requires a valid --kind"},
		{name: "invalid kind", pane: "w1:p2", args: []string{"owner", "--kind", "shell", "--handoff", "done"}, want: "requires a valid --kind"},
		{name: "unbound pane", args: []string{"owner", "--kind", "codex", "--handoff", "done"}, want: "has no bound pane"},
		{name: "unknown role", pane: "w1:p2", args: []string{"missing", "--kind", "codex", "--handoff", "done"}, want: "unknown org role"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, paths := setupOrgRecycleHome(t, test.pane)
			calls := 0
			withOrgProvider(t, orgFixtureProvider{recycle: func(context.Context, org.RecycleRequest) error {
				calls++
				return nil
			}})
			var stdout, stderr bytes.Buffer
			args := append(append([]string(nil), test.args...), "--home", home)
			if code := runOrgRecycle(args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, stdout.String(), stderr.String(), test.want)
			}
			if calls != 0 {
				t.Fatalf("provider Recycle called %d times on validation failure", calls)
			}
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			notes, err := store.ListWorkflowNotes(context.Background(), "org/owner", 0)
			if err != nil || len(notes) != 0 {
				t.Fatalf("validation failure journaled notes: %+v err=%v", notes, err)
			}
		})
	}
}

func TestRunOrgRecycleJournalsAndBootsSuccessor(t *testing.T) {
	home, paths := setupOrgRecycleHome(t, "w1:p2")
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("model = \"sonnet\"\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchOrgRolePresence(context.Background(), "owner", "agent_run"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var requests []org.RecycleRequest
	withOrgProvider(t, orgFixtureProvider{
		snapshot: org.Snapshot{
			States:     map[string]org.RoleLiveState{"owner": {State: org.StateIdle}},
			ObservedAt: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC), ProviderVersion: "0.7.5",
		},
		recycle: func(_ context.Context, req org.RecycleRequest) error {
			requests = append(requests, req)
			return nil
		},
	})
	var stdout, stderr bytes.Buffer
	code := runOrgRecycle([]string{"OWNER", "--kind", "codex", "--handoff", "Release is ready for final verification.", "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(requests) != 1 {
		t.Fatalf("Recycle requests = %+v", requests)
	}
	req := requests[0]
	if req.Role != "owner" || req.Pane != "w1:p2" || req.Kind != "codex" || req.AgentName != "owner" || req.Model != "sonnet" {
		t.Fatalf("Recycle request = %+v", req)
	}
	for _, want := range []string{"role: owner", "path: owner", "provider: idle", "last_command: agent_run", "handoff: Release is ready for final verification."} {
		if !strings.Contains(req.BootPrompt, want) {
			t.Fatalf("BootPrompt missing %q: %q", want, req.BootPrompt)
		}
	}
	var out orgRecycleOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v; output=%q", err, stdout.String())
	}
	if out.Role != "owner" || out.Pane != "w1:p2" || out.Kind != "codex" || out.AgentName != "owner" || out.WorkflowID != "org/owner" {
		t.Fatalf("output = %+v", out)
	}
	store, err = dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	notes, err := store.ListWorkflowNotes(context.Background(), "org/owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Author != "owner" || notes[0].Body != workflow.FormatOrgHandoffNote("owner", "Release is ready for final verification.") {
		t.Fatalf("handoff notes = %+v", notes)
	}
}

func TestRunOrgRecycleProviderFailureKeepsHandoff(t *testing.T) {
	home, paths := setupOrgRecycleHome(t, "w1:p2")
	withOrgProvider(t, orgFixtureProvider{
		snapshot: org.Snapshot{States: map[string]org.RoleLiveState{"owner": {State: org.StateDone}}},
		recycle:  func(context.Context, org.RecycleRequest) error { return errors.New("pane is not at shell") },
	})
	var stdout, stderr bytes.Buffer
	if code := runOrgRecycle([]string{"owner", "--kind", "codex", "--handoff", "Safe to resume.", "--home", home}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "handoff journaled in workflow org/owner") {
		t.Fatalf("code/stderr = %d/%q", code, stderr.String())
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	notes, err := store.ListWorkflowNotes(context.Background(), "org/owner", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("handoff notes after provider failure = %+v err=%v", notes, err)
	}
}

func setupOrgRecycleHome(t *testing.T, pane string) (string, config.Paths) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := "\n[org.roles.\"owner\"]\nscope = [\"*\"]\nmerge_rule = \"owner\"\n"
	if pane != "" {
		content += fmt.Sprintf("pane = %q\n", pane)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return home, paths
}

func TestValidateAndTouchActingOrgRole(t *testing.T) {
	home, paths := setupOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := validateAndTouchActingOrgRole(ctx, store, home, "review", "agent_run"); err != nil {
		t.Fatalf("validateAndTouchActingOrgRole() error = %v", err)
	}
	if err := validateAndTouchActingOrgRole(ctx, store, home, "missing", "agent_run"); err == nil {
		t.Fatal("unknown role accepted")
	}
	presence, err := store.ListOrgRolePresence(ctx)
	if err != nil || len(presence) != 1 || presence[0].Role != "review" || presence[0].LastCommand != "agent_run" {
		t.Fatalf("presence = %+v err=%v", presence, err)
	}
}

func TestValidateAndTouchActingOrgRoleRecycleEnforcement(t *testing.T) {
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name         string
		requestRole  string
		mode         string
		recycleAfter string
		seedPresence bool
		seedAt       time.Time
		wantError    bool
		wantWarning  bool
		wantTouch    bool
	}{
		{name: "block overdue", mode: "block", recycleAfter: "1h", seedPresence: true, seedAt: old, wantError: true},
		{name: "block overdue case variant", requestRole: "OWNER", mode: "block", recycleAfter: "1h", seedPresence: true, seedAt: old, wantError: true},
		{name: "warn overdue", mode: "warn", recycleAfter: "1h", seedPresence: true, seedAt: old, wantWarning: true, wantTouch: true},
		{name: "off overdue", mode: "off", recycleAfter: "1h", seedPresence: true, seedAt: old, wantTouch: true},
		{name: "default off overdue", recycleAfter: "1h", seedPresence: true, seedAt: old, wantTouch: true},
		{name: "block fresh", mode: "block", recycleAfter: "24h", seedPresence: true, seedAt: time.Now().UTC(), wantTouch: true},
		{name: "block missing presence", mode: "block", recycleAfter: "1h", wantTouch: true},
		{name: "block without recycle after", mode: "block", seedPresence: true, seedAt: old, wantTouch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, paths := setupOrgRecycleEnforcementHome(t, test.mode, test.recycleAfter)
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if test.seedPresence {
				seedOrgRolePresenceAt(t, paths, "owner", test.seedAt, "seed")
			}
			before, beforeFound, err := store.GetOrgRolePresence(context.Background(), "owner")
			if err != nil {
				t.Fatal(err)
			}

			var warning bytes.Buffer
			originalWriter := orgRecycleAdvisoryWriter
			orgRecycleAdvisoryWriter = &warning
			t.Cleanup(func() { orgRecycleAdvisoryWriter = originalWriter })
			requestRole := test.requestRole
			if requestRole == "" {
				requestRole = "owner"
			}
			err = validateAndTouchActingOrgRole(context.Background(), store, home, requestRole, "agent_run")
			if (err != nil) != test.wantError {
				t.Fatalf("validateAndTouchActingOrgRole() error = %v, wantError=%v", err, test.wantError)
			}
			if test.wantError && (!strings.Contains(err.Error(), "overdue for recycling") || !strings.Contains(err.Error(), "handoff note")) {
				t.Fatalf("block error is not actionable: %v", err)
			}
			if gotWarning := warning.String() != ""; gotWarning != test.wantWarning {
				t.Fatalf("warning = %q, wantWarning=%v", warning.String(), test.wantWarning)
			}
			if test.wantWarning && (!strings.Contains(warning.String(), "warning:") || !strings.Contains(warning.String(), "handoff note")) {
				t.Fatalf("warning is not actionable: %q", warning.String())
			}

			after, afterFound, err := store.GetOrgRolePresence(context.Background(), "owner")
			if err != nil {
				t.Fatal(err)
			}
			if !test.wantTouch {
				if afterFound != beforeFound || after != before {
					t.Fatalf("blocked dispatch touched presence: before=%+v/%v after=%+v/%v", before, beforeFound, after, afterFound)
				}
				return
			}
			if !afterFound || after.LastCommand != "agent_run" {
				t.Fatalf("allowed dispatch did not touch presence: %+v found=%v", after, afterFound)
			}
			if beforeFound && test.seedAt.Equal(old) && after.LastSeenAt == before.LastSeenAt {
				t.Fatalf("allowed overdue dispatch did not reset last_seen_at: before=%+v after=%+v", before, after)
			}
		})
	}
}

type recycleOverdueRecordingSink struct {
	t      *testing.T
	store  *db.Store
	events []events.Event
}

func (s *recycleOverdueRecordingSink) Emit(ctx context.Context, event events.Event) {
	s.t.Helper()
	episodes, err := s.store.ListRecycleOverdueEpisodes(ctx)
	if err != nil {
		s.t.Fatalf("ListRecycleOverdueEpisodes(at emit) error = %v", err)
	}
	marked := false
	for _, episode := range episodes {
		if episode.Subject == event.JobID && episode.EmittedAt != "" {
			marked = true
			break
		}
	}
	if !marked {
		s.t.Fatalf("event emitted before episode mark: event=%+v episodes=%+v", event, episodes)
	}
	s.events = append(s.events, event)
}

func TestRecycleOverdueEventBlockCadenceAndFreshClear(t *testing.T) {
	home, paths := setupOrgRecycleEnforcementHome(t, "block", "1h")
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{ID: "recycle-overdue", OnKind: "blocked", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seedOrgRolePresenceAt(t, paths, "owner", seedAt, "seed")
	sink := &recycleOverdueRecordingSink{t: t, store: store}
	originalSink := orgRecycleOverdueEventSink
	orgRecycleOverdueEventSink = func(ctx context.Context, store *db.Store, _ string) (events.Sink, error) {
		rules, err := store.ListEventRules(ctx)
		if err != nil || !hasEnabledEventRule(rules) {
			return nil, err
		}
		return sink, nil
	}
	t.Cleanup(func() { orgRecycleOverdueEventSink = originalSink })

	if err := validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run"); err == nil {
		t.Fatal("overdue block dispatch error = nil")
	}
	if len(sink.events) != 1 {
		t.Fatalf("first dispatch events = %+v, want one", sink.events)
	}
	ev := sink.events[0]
	if ev.Type != events.EventOrgRecycleOverdue || ev.JobID != "owner" || ev.RootID != "owner" || ev.Status != "overdue" || ev.Cause != "recycle_overdue" {
		t.Fatalf("recycle-overdue event = %+v", ev)
	}
	if !strings.Contains(ev.Detail, "role owner overdue for recycling") || !strings.Contains(ev.Detail, "since ") {
		t.Fatalf("recycle-overdue detail = %q", ev.Detail)
	}
	episodes, err := store.ListRecycleOverdueEpisodes(ctx)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("episodes after first dispatch = %+v err=%v", episodes, err)
	}
	if got, want := episodes[0].OverdueSince, seedAt.Add(time.Hour).Format(db.BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("OverdueSince = %q, want stable threshold %q", got, want)
	}

	if err := validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run"); err == nil {
		t.Fatal("repeat overdue block dispatch error = nil")
	}
	if len(sink.events) != 1 {
		t.Fatalf("within-interval events = %d, want 1", len(sink.events))
	}
	if err := store.MarkRecycleOverdueEpisodeEmitted(ctx, "owner", time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run"); err == nil {
		t.Fatal("post-interval overdue block dispatch error = nil")
	}
	if len(sink.events) != 2 {
		t.Fatalf("post-interval events = %d, want 2", len(sink.events))
	}
	episodes, err = store.ListRecycleOverdueEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].OverdueSince != seedAt.Add(time.Hour).Format(db.BlockedEpisodeTimeLayout) {
		t.Fatalf("repeat changed episode identity: %+v err=%v", episodes, err)
	}

	seedOrgRolePresenceAt(t, paths, "owner", time.Now().UTC(), "fresh")
	if err := validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run"); err != nil {
		t.Fatalf("fresh dispatch error = %v", err)
	}
	episodes, err = store.ListRecycleOverdueEpisodes(ctx)
	if err != nil || len(episodes) != 0 {
		t.Fatalf("fresh dispatch did not clear episode: %+v err=%v", episodes, err)
	}
}

func TestRecycleOverdueEventOffAndNoRuleAreNoOps(t *testing.T) {
	old := time.Now().UTC().Add(-2 * time.Hour)
	t.Run("enforcement off", func(t *testing.T) {
		home, paths := setupOrgRecycleEnforcementHome(t, "off", "1h")
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		seedOrgRolePresenceAt(t, paths, "owner", old, "seed")
		calls := 0
		original := orgRecycleOverdueEventSink
		orgRecycleOverdueEventSink = func(context.Context, *db.Store, string) (events.Sink, error) {
			calls++
			return &recordingSink{}, nil
		}
		t.Cleanup(func() { orgRecycleOverdueEventSink = original })
		if err := validateAndTouchActingOrgRole(context.Background(), store, home, "owner", "agent_run"); err != nil {
			t.Fatalf("off dispatch error = %v", err)
		}
		if calls != 0 {
			t.Fatalf("off enforcement constructed event sink %d times", calls)
		}
	})

	t.Run("no enabled event rule", func(t *testing.T) {
		home, paths := setupOrgRecycleEnforcementHome(t, "block", "1h")
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		seedOrgRolePresenceAt(t, paths, "owner", old, "seed")
		if err := validateAndTouchActingOrgRole(context.Background(), store, home, "owner", "agent_run"); err == nil {
			t.Fatal("block dispatch error = nil")
		}
		episodes, err := store.ListRecycleOverdueEpisodes(context.Background())
		if err != nil || len(episodes) != 0 {
			t.Fatalf("no-rule dispatch created episodes: %+v err=%v", episodes, err)
		}
	})

	t.Run("fresh dispatch clears a stale episode without a rule", func(t *testing.T) {
		home, paths := setupOrgRecycleEnforcementHome(t, "block", "1h")
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()
		// A prior episode (e.g. from when a rule was enabled) must be cleared by a
		// later fresh dispatch even with no rule now, so a re-overdue opens fresh.
		if err := store.UpsertRecycleOverdueEpisode(ctx, "owner", old, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		seedOrgRolePresenceAt(t, paths, "owner", time.Now().UTC(), "seed")
		if err := validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run"); err != nil {
			t.Fatalf("fresh dispatch error = %v", err)
		}
		episodes, err := store.ListRecycleOverdueEpisodes(ctx)
		if err != nil || len(episodes) != 0 {
			t.Fatalf("fresh dispatch left a stale episode (clear must run without a rule): %+v err=%v", episodes, err)
		}
	})
}

func TestRecycleOverdueEventFailurePreservesEnforcementOutcome(t *testing.T) {
	for _, mode := range []string{"block", "warn"} {
		t.Run(mode, func(t *testing.T) {
			home, paths := setupOrgRecycleEnforcementHome(t, mode, "1h")
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			seedOrgRolePresenceAt(t, paths, "owner", time.Now().UTC().Add(-2*time.Hour), "seed")
			originalSink := orgRecycleOverdueEventSink
			originalEmitter := orgRecycleOverdueEpisodeEmitter
			originalEventWriter := orgRecycleOverdueEventWriter
			orgRecycleOverdueEventSink = func(context.Context, *db.Store, string) (events.Sink, error) { return &recordingSink{}, nil }
			orgRecycleOverdueEpisodeEmitter = func(context.Context, *db.Store, events.Sink, db.RecycleOverdueEpisode, time.Duration, time.Time) error {
				return errors.New("synthetic emit failure")
			}
			var eventWarnings bytes.Buffer
			orgRecycleOverdueEventWriter = &eventWarnings
			t.Cleanup(func() {
				orgRecycleOverdueEventSink = originalSink
				orgRecycleOverdueEpisodeEmitter = originalEmitter
				orgRecycleOverdueEventWriter = originalEventWriter
			})

			var advisory bytes.Buffer
			originalAdvisory := orgRecycleAdvisoryWriter
			orgRecycleAdvisoryWriter = &advisory
			t.Cleanup(func() { orgRecycleAdvisoryWriter = originalAdvisory })
			err = validateAndTouchActingOrgRole(ctx, store, home, "owner", "agent_run")
			if (err != nil) != (mode == "block") {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
			if !strings.Contains(eventWarnings.String(), "synthetic emit failure") {
				t.Fatalf("event failure was not logged: %q", eventWarnings.String())
			}
			presence, found, readErr := store.GetOrgRolePresence(ctx, "owner")
			if readErr != nil || !found {
				t.Fatalf("presence = %+v found=%v err=%v", presence, found, readErr)
			}
			if mode == "warn" && presence.LastCommand != "agent_run" {
				t.Fatalf("warn emit failure changed allow/touch outcome: %+v", presence)
			}
			if mode == "block" && presence.LastCommand != "seed" {
				t.Fatalf("block emit failure changed refuse/no-touch outcome: %+v", presence)
			}
		})
	}
}

func setupOrgRecycleEnforcementHome(t *testing.T, mode, recycleAfter string) (string, config.Paths) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("\n[org]\n")
	if mode != "" {
		fmt.Fprintf(&body, "recycle_enforce = %q\n", mode)
	}
	if recycleAfter != "" {
		fmt.Fprintf(&body, "recycle_after = %q\n", recycleAfter)
	}
	body.WriteString("[org.roles.\"owner\"]\nscope = [\"*\"]\n")
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body.String()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return home, paths
}

func seedOrgRolePresenceAt(t *testing.T, paths config.Paths, role string, at time.Time, command string) {
	t.Helper()
	raw, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO org_role_presence(role, last_seen_at, last_command)
		VALUES (?, ?, ?)
		ON CONFLICT(role) DO UPDATE SET last_seen_at = excluded.last_seen_at, last_command = excluded.last_command`, role, at.UTC().Format("2006-01-02 15:04:05"), command); err != nil {
		t.Fatal(err)
	}
}

func TestRunOrgInitScaffoldsAndRunsHerdrGate(t *testing.T) {
	home := t.TempDir()
	originalRunner := orgDoctorRunner
	orgDoctorRunner = orgFixtureRunner{version: "herdr 0.7.5\n"}
	t.Cleanup(func() { orgDoctorRunner = originalRunner })
	withOrgProvider(t, orgFixtureProvider{snapshot: org.Snapshot{
		States:     map[string]org.RoleLiveState{"owner": {State: org.StateUnknown}},
		ObservedAt: time.Now().UTC(), ProviderVersion: "0.7.5",
	}})
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init code = %d stderr=%s", code, stderr.String())
	}
	cfg, err := config.LoadOrg(config.PathsForHome(home))
	if err != nil || !cfg.Enabled() {
		t.Fatalf("registry = %+v err=%v", cfg, err)
	}
	if !strings.Contains(stdout.String(), "herdr 0.7.5") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunOrgInitRejectsOldHerdrLoudly(t *testing.T) {
	home := t.TempDir()
	originalRunner := orgDoctorRunner
	orgDoctorRunner = orgFixtureRunner{version: "herdr 0.7.4\n"}
	t.Cleanup(func() { orgDoctorRunner = originalRunner })
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "init", "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "org requires herdr >=0.7.5") {
		t.Fatalf("init code/stderr = %d/%q", code, stderr.String())
	}
	cfg, err := config.LoadOrg(config.PathsForHome(home))
	if err != nil || !cfg.Enabled() {
		t.Fatalf("scaffold should remain inspectable after gate failure: registry=%+v err=%v", cfg, err)
	}
}

func TestRunOrgMalformedConfigFailsClosedWithoutPresence(t *testing.T) {
	home, paths := setupOrgHome(t)
	if err := os.WriteFile(paths.ConfigFile, []byte("[org]\nenforce = \"invalid\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"org", "brief", "--home", home, "--role", "owner"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "load org registry") {
		t.Fatalf("brief code/stderr = %d/%q", code, stderr.String())
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	presence, err := store.ListOrgRolePresence(context.Background())
	if err != nil || len(presence) != 0 {
		t.Fatalf("presence = %+v err=%v", presence, err)
	}
}

func TestParseAgentOrgRoleFlags(t *testing.T) {
	var stderr bytes.Buffer
	ask, ok := parseAgentAskOptions([]string{"agent", "message", "--org-role", "owner"}, &stderr)
	if !ok || ask.orgRole != "owner" {
		t.Fatalf("ask = %+v ok=%v stderr=%s", ask, ok, stderr.String())
	}
	stderr.Reset()
	run, ok := parseAgentRunOptions("run", []string{"agent", "message", "--org-role=review"}, &stderr)
	if !ok || run.orgRole != "review" {
		t.Fatalf("run = %+v ok=%v stderr=%s", run, ok, stderr.String())
	}
}

func TestOrgEventRuleAddListRemoveAndValidation(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n[org.roles.\"maintainer\"]\nparent=\"owner\"\nscope=[\"*\"]\npane=\"w1:p2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "attention", "--match", "Acme/Widget", "--wake", "MAINTAINER"}, &out, &errOut); code != 0 {
		t.Fatalf("add code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	id := strings.TrimSpace(strings.TrimPrefix(out.String(), "added "))
	if !strings.HasPrefix(id, "event-rule-") {
		t.Fatalf("add output=%q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "list", "--home", home}, &out, &errOut); code != 0 {
		t.Fatalf("list code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	for _, want := range []string{id, "on=attention", "match=Acme/Widget", "wake=maintainer", "scope=addressed", "enabled=true"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list output %q missing %q", out.String(), want)
		}
	}
	out.Reset()
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "rm", "--home", home, id}, &out, &errOut); code != 0 {
		t.Fatalf("rm code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "list", "--home", home}, &out, &errOut); code != 0 || out.Len() != 0 {
		t.Fatalf("list after rm code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "review-verdict", "--wake", "owner", "--scope", "observer"}, &out, &errOut); code != 0 {
		t.Fatalf("add review-verdict code=%d out=%q err=%q", code, out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "add", "--on", "surprise", "--wake", "owner"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown event rule kind") {
		t.Fatalf("unknown kind code=%d err=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "attention", "--wake", "owner", "--scope", "broadcast"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown event rule scope") {
		t.Fatalf("unknown scope code=%d err=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "guard", "--wake", "unknown"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown org role") {
		t.Fatalf("unknown role code=%d err=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := runOrg([]string{"events", "rule", "rm", id, "--home", home}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "place --home before the id") {
		t.Fatalf("post-positional --home code=%d err=%q", code, errOut.String())
	}
}

func TestOrgEventRuleSetScopeRoundTrip(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "blocked", "--wake", "owner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	id := strings.TrimSpace(strings.TrimPrefix(stdout.String(), "added "))
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"events", "rule", "set-scope", "--home", home, id, "observer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("set-scope observer code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if want := "updated " + id + " scope=observer\n"; stdout.String() != want {
		t.Fatalf("set-scope output=%q, want %q", stdout.String(), want)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Scope != db.EventRuleScopeObserver {
		t.Fatalf("rules = %+v, want observer scope", rules)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"events", "rule", "set-scope", "--home", home, id, "addressed"}, &stdout, &stderr); code != 0 {
		t.Fatalf("set-scope addressed code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	rules, err = store.ListEventRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Scope != db.EventRuleScopeAddressed {
		t.Fatalf("rules = %+v, want addressed scope", rules)
	}
}

func TestOrgEventRuleRepoAliasUsesMatchFilter(t *testing.T) {
	home, paths := setupOrgHome(t)
	add := func(flagName string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "blocked", flagName, "tendwire", "--wake", "owner"}, &stdout, &stderr); code != 0 {
			t.Fatalf("add %s code=%d out=%q err=%q", flagName, code, stdout.String(), stderr.String())
		}
		return strings.TrimSpace(strings.TrimPrefix(stdout.String(), "added "))
	}
	matchID := add("--match")
	repoID := add("--repo")

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("event rules = %d, want 2", len(rules))
	}
	byID := make(map[string]db.EventRule, len(rules))
	for _, rule := range rules {
		byID[rule.ID] = rule
	}
	matchRule, matchOK := byID[matchID]
	repoRule, repoOK := byID[repoID]
	if !matchOK || !repoOK {
		t.Fatalf("created rules missing: ids=%q,%q rules=%+v", matchID, repoID, rules)
	}
	if matchRule.OnKind != repoRule.OnKind ||
		matchRule.MatchFilter != repoRule.MatchFilter ||
		matchRule.WakeRole != repoRule.WakeRole ||
		matchRule.Enabled != repoRule.Enabled ||
		repoRule.MatchFilter != "tendwire" {
		t.Fatalf("--match rule = %+v, --repo rule = %+v", matchRule, repoRule)
	}

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"events", "rule", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	lineSuffixes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		id, suffix, ok := strings.Cut(line, "\t")
		if ok {
			lineSuffixes[id] = suffix
		}
	}
	if lineSuffixes[matchID] == "" || lineSuffixes[matchID] != lineSuffixes[repoID] || !strings.Contains(lineSuffixes[repoID], "match=tendwire") {
		t.Fatalf("list output differs by input alias: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "blocked", "--match", "tendwire", "--repo", "gitmoot/", "--wake", "owner"}, &stdout, &stderr); code != 2 {
		t.Fatalf("both filters code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--match and --repo cannot both be set") {
		t.Fatalf("both filters error = %q", stderr.String())
	}
	rules, err = store.ListEventRules(context.Background())
	if err != nil || len(rules) != 2 {
		t.Fatalf("ambiguous add changed rules: rules=%+v err=%v", rules, err)
	}
}
