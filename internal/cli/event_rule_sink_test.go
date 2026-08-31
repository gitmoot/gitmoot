package cli

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type fakeEventWake struct {
	availableCalls int
	promptCalls    int
	pane           string
	prompt         string
	panes          []string
	prompts        []string
	until          string
	labelToPane    map[string]string
	stalled        bool
	promptErr      error
	oddNonDelivery bool
	onPrompt       func() error
}

func (f *fakeEventWake) Available(context.Context) bool {
	f.availableCalls++
	return true
}

func (f *fakeEventWake) AgentPrompt(_ context.Context, pane, prompt, until string) (bool, bool, error) {
	f.promptCalls++
	f.pane, f.prompt, f.until = pane, prompt, until
	f.panes = append(f.panes, pane)
	f.prompts = append(f.prompts, prompt)
	if f.onPrompt != nil {
		if err := f.onPrompt(); err != nil {
			return false, false, err
		}
	}
	if f.stalled {
		return false, true, nil
	}
	if f.promptErr != nil {
		return false, false, f.promptErr
	}
	if f.oddNonDelivery {
		return false, false, nil
	}
	return true, false, nil
}

func (f *fakeEventWake) ResolvePaneByLabel(_ context.Context, label string) (string, bool) {
	pane, ok := f.labelToPane[label]
	if !ok && strings.Contains(label, ":") {
		return label, true
	}
	return pane, ok
}

func TestEventRuleDirectiveWakePromptMatchesCurrentPhase(t *testing.T) {
	base := events.Event{
		RootID:         db.WakeOutboxSourceWorkflowNote + ":42",
		WakeTargetRole: "worker",
	}
	tests := []struct {
		name      string
		cause     string
		want      []string
		forbidden []string
	}{
		{
			name:      "receipt",
			cause:     "addressed_directive",
			want:      []string{"acknowledge receipt", "gitmoot org directive ack 42 --by worker"},
			forbidden: []string{"gitmoot org directive done"},
		},
		{
			// MUTANT: reverting to the fixed receipt prompt would tell an already
			// acknowledged recipient to append another ack instead of delivering.
			name:      "completion",
			cause:     directiveCompletionOverdueCause,
			want:      []string{"finish the assigned deliverable", "gitmoot org directive done 42 --by worker"},
			forbidden: []string{"gitmoot org directive ack", "acknowledge receipt"},
		},
		{
			// MUTANT: a stale pending wake for a terminal directive must never
			// render either state-changing command.
			name:      "terminal",
			cause:     directiveTerminalCause,
			want:      []string{"already terminal", "no receipt or completion action is required"},
			forbidden: []string{"gitmoot org directive ack", "gitmoot org directive done"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := base
			event.Cause = tt.cause
			prompt := eventRuleWakePrompt(db.WakeOutboxKindDirective, event)
			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt %q does not contain %q", prompt, want)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("prompt %q contains forbidden %q", prompt, forbidden)
				}
			}
		})
	}
}
func TestEventRuleReviewVerdictWakePrompt(t *testing.T) {
	event := events.Event{
		JobID:          "review-42",
		Repo:           "gitmoot/gitmoot",
		Detail:         "one blocking issue",
		PullRequest:    42,
		ReviewDecision: "changes_requested",
	}
	prompt := eventRuleWakePrompt(eventRuleKindReviewVerdict, event)
	for _, want := range []string{"changes_requested", "gitmoot/gitmoot#42", "review-42", "one blocking issue"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q does not contain %q", prompt, want)
		}
	}
}

func TestClassifyEventRuleKinds(t *testing.T) {
	tests := []struct {
		name  string
		event events.Event
		want  []string
	}{
		{name: "escalation", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "escalation"}, want: []string{"escalation"}},
		{name: "attention", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "ask_gate"}, want: []string{"attention"}},
		{name: "merge guard", event: events.Event{Type: events.EventJobBlocked, Cause: "merge_guard"}, want: []string{"guard"}},
		{name: "permission guard", event: events.Event{Type: events.EventJobBlocked, Cause: "permission_guard"}, want: []string{"guard"}},
		{name: "blocked since only", event: events.Event{Type: events.EventJobBlocked, Cause: "blocked_since"}, want: []string{"blocked"}},
		{name: "recycle overdue", event: events.Event{Type: events.EventOrgRecycleOverdue, Cause: "recycle_overdue"}, want: []string{"recycle-overdue"}},
		{name: "pane input pending", event: events.Event{Type: events.EventOrgInputPending, Cause: "input_pending_since"}, want: []string{"pane_input_pending"}},
		{name: "addressed reply", event: events.Event{Type: events.EventOrgReply, Cause: "addressed_note"}, want: []string{"reply"}},
		{name: "addressed directive", event: events.Event{Type: events.EventOrgDirective, Cause: "addressed_directive"}, want: []string{"directive"}},
		{name: "addressed awaited fact", event: events.Event{Type: events.EventOrgFact, Cause: "awaited_fact_satisfied"}, want: []string{"fact"}},
		{name: "finished terminal", event: events.Event{Type: events.EventJobFinished}, want: []string{"job-terminal"}},
		{name: "failed terminal", event: events.Event{Type: events.EventJobFailed, Cause: "unrelated"}, want: []string{"job-terminal"}},
		{name: "review verdict", event: events.Event{Type: events.EventJobFinished, Cause: events.EventCauseReviewVerdict}, want: []string{"job-terminal", eventRuleKindReviewVerdict}},
		{name: "plain blocked terminal and blocked", event: events.Event{Type: events.EventJobBlocked}, want: []string{"job-terminal", "blocked"}},
		{name: "advance blocked terminal and blocked", event: events.Event{Type: events.EventJobBlocked, Cause: "advance_blocked"}, want: []string{"job-terminal", "blocked"}},
		{name: "unknown blocked cause", event: events.Event{Type: events.EventJobBlocked, Cause: "other"}},
		{name: "unknown attention cause", event: events.Event{Type: events.EventJobNeedsAttention, Cause: "other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyEventRuleKinds(tt.event); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestWakeTargetRoleProductionWritesMatchObserverRegistry(t *testing.T) {
	writes := productionWakeTargetRoleWrites(t)
	if got, want := fmt.Sprint(writes), "[internal/cli/blocked_since.go:buildDirectiveEscalationEvent internal/cli/blocked_since.go:buildDirectiveNudgeEvent internal/cli/blocked_since.go:emitInputPendingEpisode internal/cli/event_rule_sink.go:addressBlockedEvent internal/cli/event_sink.go:emitDaemonTerminalEvent internal/cli/reply_wake_outbox.go:wakeOutboxEvent internal/daemon/task_disposal.go:strandTask internal/workflow/engine_types.go:mailbox]"; got != want {
		t.Fatalf("production WakeTargetRole writes = %s, want %s", got, want)
	}
	registryWrites := make([]wakeTargetRoleWrite, 0, len(wakeTargetRoleProducers))
	for _, producer := range wakeTargetRoleProducers {
		registryWrites = append(registryWrites, wakeTargetRoleWrite{
			file: producer.File, function: producer.Function,
		})
	}
	sort.Slice(registryWrites, func(i, j int) bool {
		if registryWrites[i].file != registryWrites[j].file {
			return registryWrites[i].file < registryWrites[j].file
		}
		return registryWrites[i].function < registryWrites[j].function
	})
	if got, want := fmt.Sprint(registryWrites), fmt.Sprint(writes); got != want {
		t.Fatalf("WakeTargetRole producer registry = %s, production writes = %s", got, want)
	}
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{{
		SourceKind: "workflow_note", SourceID: "note-1", TargetRole: "owner",
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != events.EventOrgReply || strings.TrimSpace(event.WakeTargetRole) != "owner" {
		t.Fatalf("reply wake event = {Type:%q WakeTargetRole:%q}, want org.reply addressed to owner", event.Type, event.WakeTargetRole)
	}
}

type wakeTargetRoleWrite struct {
	file     string
	function string
}

func (w wakeTargetRoleWrite) String() string {
	return w.file + ":" + w.function
}

func productionWakeTargetRoleWrites(t *testing.T) []wakeTargetRoleWrite {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var writes []wakeTargetRoleWrite
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(filename) != ".go" || strings.HasSuffix(filename, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			functionName := "<package>"
			node := ast.Node(declaration)
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Body == nil {
					continue
				}
				functionName = function.Name.Name
				node = function.Body
			}
			ast.Inspect(node, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.AssignStmt:
					for _, expression := range node.Lhs {
						selector, ok := expression.(*ast.SelectorExpr)
						if ok && selector.Sel.Name == "WakeTargetRole" {
							writes = append(writes, wakeTargetRoleWrite{
								file: filepath.ToSlash(relative), function: functionName,
							})
						}
					}
				case *ast.KeyValueExpr:
					key, ok := node.Key.(*ast.Ident)
					if ok && key.Name == "WakeTargetRole" {
						writes = append(writes, wakeTargetRoleWrite{
							file: filepath.ToSlash(relative), function: functionName,
						})
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(writes, func(i, j int) bool {
		if writes[i].file != writes[j].file {
			return writes[i].file < writes[j].file
		}
		return writes[i].function < writes[j].function
	})
	return writes
}

func TestRolePaneResolverHasAllThreeProductionCallSites(t *testing.T) {
	calls := productionSelectorCallSites(t, "ResolveRolePaneBinding")
	want := []wakeTargetRoleWrite{
		{file: "internal/cli/event_rule_sink.go", function: "resolveRolePane"},
		{file: "internal/cli/org_role_unavailable.go", function: "wakeParent"},
		{file: "internal/cockpit/herdr_org.go", function: "Snapshot"},
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].file != want[j].file {
			return want[i].file < want[j].file
		}
		return want[i].function < want[j].function
	})
	if got, expected := fmt.Sprint(calls), fmt.Sprint(want); got != expected {
		t.Fatalf("ResolveRolePaneBinding call sites = %s, want %s", got, expected)
	}
}

func productionSelectorCallSites(t *testing.T, selectorName string) []wakeTargetRoleWrite {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var calls []wakeTargetRoleWrite
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(filename) != ".go" || strings.HasSuffix(filename, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == selectorName {
					calls = append(calls, wakeTargetRoleWrite{
						file: filepath.ToSlash(relative), function: function.Name.Name,
					})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].file != calls[j].file {
			return calls[i].file < calls[j].file
		}
		return calls[i].function < calls[j].function
	})
	return calls
}

func TestJobTerminalAddressedScopeMatchesDispatcherOnly(t *testing.T) {
	author := db.EventRule{WakeRole: "author", Scope: db.EventRuleScopeAddressed}
	other := db.EventRule{WakeRole: "other", Scope: db.EventRuleScopeAddressed}
	observer := db.EventRule{WakeRole: "auditor", Scope: db.EventRuleScopeObserver}
	addressed := events.Event{Type: events.EventJobFinished, Cause: events.EventCauseReviewVerdict, WakeTargetRole: "author"}
	if !eventRuleMatchesAddressee(author, addressed) ||
		eventRuleMatchesAddressee(other, addressed) ||
		!eventRuleMatchesAddressee(observer, addressed) {
		t.Fatal("addressed terminal event did not match author and observer only")
	}
	roleless := events.Event{Type: events.EventJobFinished}
	if eventRuleMatchesAddressee(author, roleless) ||
		eventRuleMatchesAddressee(other, roleless) ||
		!eventRuleMatchesAddressee(observer, roleless) {
		t.Fatal("roleless terminal event did not match observer only")
	}
}

func TestEventRuleAddresseeGateIsKindAgnosticAndObserverExempt(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `
[org.roles."owner"]
scope=["*"]
pane="w1:p1"
[org.roles."other"]
parent="owner"
scope=["*"]
pane="w1:p2"
[org.roles."auditor"]
parent="owner"
scope=["*"]
pane="w1:p3"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	rules := []db.EventRule{
		{ID: "target", OnKind: "escalation", WakeRole: "owner", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "wrong-target", OnKind: "escalation", WakeRole: "other", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "observer", OnKind: "escalation", WakeRole: "auditor", Scope: db.EventRuleScopeObserver, Enabled: true},
	}
	event := events.Event{
		Type: events.EventJobNeedsAttention, Cause: "escalation",
		JobID: "job-1", WakeTargetRole: "owner",
	}
	if err := sink.evaluateRules(context.Background(), event, rules); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(wake.panes, ","), "w1:p1,w1:p3"; got != want {
		t.Fatalf("woken panes = %q, want %q", got, want)
	}
}

func TestAddresslessBlockedWakeMatchesObserversOnly(t *testing.T) {
	store := daemonWorkerStore(t)
	ctx := context.Background()
	for _, rule := range []db.EventRule{
		{ID: "addressed-uninvolved", OnKind: "blocked", WakeRole: "uninvolved", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "observer-jarvis", OnKind: "blocked", WakeRole: "jarvis", Scope: db.EventRuleScopeObserver, Enabled: true},
		{ID: "observer-audit", OnKind: "blocked", WakeRole: "audit", Scope: db.EventRuleScopeObserver, Enabled: true},
	} {
		if err := store.AddEventRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}

	(&eventRuleSink{store: store}).Emit(ctx, events.Event{
		Type: events.EventJobBlocked, Cause: "blocked_since", JobID: "task-without-owner",
	})

	if got, want := pendingWakeTargetRoles(t, store), []string{"audit", "jarvis"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("address-less blocked wake targets = %v, want observer roles %v", got, want)
	}
}

func TestBlockedWakeTargetsRecordedOrResolvedOwnerPlusObservers(t *testing.T) {
	tests := []struct {
		name        string
		repo        string
		jobRole     string
		wantRoles   []string
		wantTarget  string
		useTerminal bool
	}{
		{name: "terminal uses recorded workflow role", repo: "gitmoot/gitmoot", jobRole: "lane", useTerminal: true, wantRoles: []string{"jarvis", "lane"}, wantTarget: "lane"},
		{name: "blocked-since resolves repo owner", repo: "gitmoot/gitmoot", wantRoles: []string{"jarvis", "lane"}, wantTarget: "lane"},
		{name: "repo without owner is observers only", repo: "unknown/repo", wantRoles: []string{"jarvis"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatal(err)
			}
			const orgConfig = `
[org.roles."owner"]
scope=["gitmoot/*", "other/*", "oversight/*"]
pane="w1:p1"
[org.roles."lane"]
parent="owner"
scope=["gitmoot/gitmoot"]
pane="w1:p2"
[org.roles."other"]
parent="owner"
scope=["other/*"]
pane="w1:p3"
[org.roles."jarvis"]
parent="owner"
scope=["oversight/*"]
pane="w1:p4"
`
			if err := os.WriteFile(paths.ConfigFile, []byte(orgConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			for _, rule := range []db.EventRule{
				{ID: "addressed-lane", OnKind: "blocked", WakeRole: "lane", Scope: db.EventRuleScopeAddressed, Enabled: true},
				{ID: "addressed-other", OnKind: "blocked", WakeRole: "other", Scope: db.EventRuleScopeAddressed, Enabled: true},
				{ID: "observer-jarvis", OnKind: "blocked", WakeRole: "jarvis", Scope: db.EventRuleScopeObserver, Enabled: true},
			} {
				if err := store.AddEventRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
			}
			recorded := &recordingSink{}
			sink := &eventRuleSink{inner: recorded, store: store, home: home}
			if test.useTerminal {
				if err := store.CreateJob(ctx, db.Job{
					ID: "blocked-job", Agent: "worker", Type: "ask", State: string(workflow.JobBlocked),
					Payload: mustJobPayload(t, workflow.JobPayload{Repo: test.repo, ActingOrgRole: test.jobRole}),
				}); err != nil {
					t.Fatal(err)
				}
				emitDaemonTerminalEvent(ctx, sink, store, "blocked-job", daemonTerminalBlocked, string(workflow.JobBlocked), "blocked")
			} else {
				sink.Emit(ctx, events.Event{Type: events.EventJobBlocked, Cause: "blocked_since", JobID: "blocked-task", Repo: test.repo})
			}
			if got := pendingWakeTargetRoles(t, store); !reflect.DeepEqual(got, test.wantRoles) {
				t.Fatalf("blocked wake targets = %v, want %v", got, test.wantRoles)
			}
			gotEvents := recorded.byType(events.EventJobBlocked)
			if len(gotEvents) != 1 || gotEvents[0].WakeTargetRole != test.wantTarget {
				t.Fatalf("blocked event target = %+v, want %q", gotEvents, test.wantTarget)
			}
		})
	}
}

func pendingWakeTargetRoles(t *testing.T, store *db.Store) []string {
	t.Helper()
	rows, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	roles := make([]string, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, row.TargetRole)
	}
	sort.Strings(roles)
	return roles
}

func TestReplyObserverDoesNotConsumeAddressedTargetAttempt(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `
[org.roles."owner"]
scope=["*"]
pane="w1:p1"
[org.roles."auditor"]
parent="owner"
scope=["*"]
pane="w1:p2"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	rules := []db.EventRule{
		{ID: "observer", OnKind: "reply", WakeRole: "auditor", Scope: db.EventRuleScopeObserver, Enabled: true},
		{ID: "target", OnKind: "reply", WakeRole: "owner", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "target-duplicate", OnKind: "reply", WakeRole: "owner", Scope: db.EventRuleScopeAddressed, Enabled: true},
	}
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{{
		SourceKind: "workflow_note", SourceID: "note-1", TargetRole: "owner",
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.evaluateRules(context.Background(), event, rules); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(wake.panes, ","), "w1:p2,w1:p1"; got != want {
		t.Fatalf("woken panes = %q, want %q", got, want)
	}
}

func TestEventRuleMatchUsesExactRepositoryAndSubstringJobID(t *testing.T) {
	event := events.Event{Repo: "Acme/Widget", JobID: "Job-AbC"}
	for _, filter := range []string{"", "acme/widget", "WIDGET", "job-a", "ABC"} {
		if !eventRuleMatches(filter, event) {
			t.Fatalf("filter %q did not match", filter)
		}
	}
	for _, filter := range []string{"acme/w", "acme/widget-plus", "missing"} {
		if eventRuleMatches(filter, event) {
			t.Fatalf("filter %q unexpectedly matched", filter)
		}
	}
	delegated := events.Event{Repo: "owner/repo", JobID: "root/delegation/check"}
	for _, filter := range []string{"root/delegation/check", "delegation/check"} {
		if !eventRuleMatches(filter, delegated) {
			t.Fatalf("slash-bearing job-id filter %q did not match %q", filter, delegated.JobID)
		}
	}
	prefixCollision := events.Event{Repo: "acme/widget-plus", JobID: "other"}
	if eventRuleMatches("acme/widget", prefixCollision) {
		t.Fatal("exact repo filter matched a prefix-colliding repository")
	}
}

func TestEventRuleEvaluatorResolvesPaneAndWakes(t *testing.T) {
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
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-1", OnKind: "attention", MatchFilter: "WIDGET", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobNeedsAttention, Cause: "ask_gate", Repo: "acme/widget", JobID: "job-1", Detail: "Please choose", WakeTargetRole: "owner"})
	if wake.availableCalls != 1 || wake.pane != "w1:p1" || wake.until != "" {
		t.Fatalf("wake=%+v", wake)
	}
	if want := "gitmoot attention event for job job-1: Please choose"; wake.prompt != want {
		t.Fatalf("prompt=%q want=%q", wake.prompt, want)
	}
}

func TestEventRuleWakeStallIncrementsAndDeliveryResetsCounter(t *testing.T) {
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
	defer store.Close()
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{ID: "rule-counter", OnKind: "attention", WakeRole: "OWNER", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{stalled: true}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	event := events.Event{Type: events.EventJobNeedsAttention, Cause: "ask_gate", JobID: "job-counter", WakeTargetRole: "owner"}
	sink.evaluate(ctx, event)
	misses, err := store.ListRoleMissedWakes(ctx)
	if err != nil || len(misses) != 1 || misses[0].Role != "owner" || misses[0].Consecutive != 1 {
		t.Fatalf("misses after stalled wake = %+v, err=%v", misses, err)
	}

	wake.stalled = false
	sink.evaluate(ctx, event)
	misses, err = store.ListRoleMissedWakes(ctx)
	if err != nil || len(misses) != 0 {
		t.Fatalf("misses after delivered wake = %+v, err=%v", misses, err)
	}
}

func TestEventRuleEvaluatorResolvesPaneLabel(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	// A binding without ':' is a herdr pane LABEL, resolved to the live id at wake time.
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"coordinator-a\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-lbl", OnKind: "guard", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{labelToPane: map[string]string{"coordinator-a": "w2:p5"}}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobBlocked, Cause: "merge_guard", JobID: "job-9", WakeTargetRole: "owner"})
	if wake.pane != "w2:p5" {
		t.Fatalf("label did not resolve to live pane id: %+v", wake)
	}
}

func TestEventRuleEvaluatorCountsUnresolvedRoleBinding(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"missing-label\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "rule-unresolved", OnKind: "attention", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	if err := sink.evaluate(context.Background(), events.Event{
		Type: events.EventJobNeedsAttention, Cause: "ask_gate", JobID: "job-unresolved", WakeTargetRole: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	missed, err := store.ListRoleMissedWakes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(missed) != 1 || missed[0].Role != "owner" || missed[0].Consecutive != 1 {
		t.Fatalf("unresolved binding missed wakes = %+v, want owner count 1", missed)
	}
	if wake.promptCalls != 0 {
		t.Fatalf("unresolved binding prompted %d times", wake.promptCalls)
	}
}

func TestDurableWakeWithUnresolvedPaneParksAsStalled(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"missing-pane\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "blocked-owner", OnKind: "blocked", WakeRole: "owner", Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	ruleSink := &eventRuleSink{store: store, home: home, wake: &fakeEventWake{}}
	ruleSink.Emit(ctx, events.Event{
		Type: events.EventJobBlocked, Cause: "blocked_since", JobID: "blocked-task", Repo: "owner/repo", WakeTargetRole: "owner",
	})
	if err := drainReplyWakeAfterAllRowsAreDueResult(t, store, synchronousEventRuleTestSink{sink: ruleSink}); err != nil {
		t.Fatalf("drain unresolved pane wake: %v", err)
	}
	stalled, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateStalled)
	if err != nil || len(stalled) != 1 || stalled[0].TargetRole != "owner" || stalled[0].LastError != "role pane binding unresolved" {
		t.Fatalf("stalled unresolved wake = %+v, err=%v", stalled, err)
	}
	failed, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateFailed)
	if err != nil || len(failed) != 0 {
		t.Fatalf("failed unresolved wake rows = %+v, err=%v", failed, err)
	}
	missed, err := store.ListRoleMissedWakes(ctx)
	if err != nil || len(missed) != 1 || missed[0].Role != "owner" || missed[0].Consecutive != 1 {
		t.Fatalf("unresolved wake counter = %+v, err=%v", missed, err)
	}
}

func TestEventRuleEvaluatorZeroRulesDoesNotProbeHerdr(t *testing.T) {
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{}
	(&eventRuleSink{store: store, wake: wake}).evaluate(context.Background(), events.Event{Type: events.EventJobFinished, JobID: "job-1"})
	if wake.availableCalls != 0 {
		t.Fatalf("availability probed %d times with zero rules", wake.availableCalls)
	}
}

func TestDaemonEventSinkRuleOnlyActivationAndRemoval(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if sink := daemonEventSink(store, paths.Home); sink != nil {
		t.Fatal("zero rules and no webhook must produce a nil sink")
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "rule-activate", OnKind: "guard", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if sink := daemonEventSink(store, paths.Home); sink == nil {
		t.Fatal("enabled rule must activate the sink without a webhook")
	}
	if err := store.DeleteEventRule(context.Background(), "rule-activate"); err != nil {
		t.Fatal(err)
	}
	if sink := daemonEventSink(store, paths.Home); sink != nil {
		t.Fatal("removing the last rule must restore the nil off path")
	}
}

// TestEventRuleWakeFiresEachMatchingRule guards the multi-rule fan-out: a plain
// job.blocked event classifies to BOTH job-terminal and blocked, so two rules —
// one per kind — must each produce a wake. (The per-rule wake context in evaluate
// is what keeps a slow earlier wake from starving the later one; see #1060.)
func TestEventRuleWakeFiresEachMatchingRule(t *testing.T) {
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
	defer store.Close()
	for _, r := range []db.EventRule{
		{ID: "r-term", OnKind: "job-terminal", WakeRole: "owner", Enabled: true},
		{ID: "r-blk", OnKind: "blocked", WakeRole: "owner", Enabled: true},
	} {
		if err := store.AddEventRule(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{Type: events.EventJobBlocked, JobID: "job-1", WakeTargetRole: "owner"})
	if wake.promptCalls != 2 {
		t.Fatalf("want a wake for each of the 2 matching rules, got %d", wake.promptCalls)
	}
}
func TestReviewVerdictWakeTargetsRequesterImplementerAndObservers(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `
[org.roles."owner"]
scope=["*"]
pane="w1:p0"
[org.roles."author"]
parent="owner"
scope=["*"]
pane="w1:p1"
[org.roles."requester"]
parent="owner"
scope=["*"]
pane="w1:p4"
[org.roles."other"]
parent="owner"
scope=["*"]
pane="w1:p2"
[org.roles."auditor"]
parent="owner"
scope=["*"]
pane="w1:p3"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, rule := range []db.EventRule{
		{ID: "author", OnKind: eventRuleKindReviewVerdict, WakeRole: "author", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "requester", OnKind: eventRuleKindReviewVerdict, WakeRole: "requester", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "other", OnKind: eventRuleKindReviewVerdict, WakeRole: "other", Scope: db.EventRuleScopeAddressed, Enabled: true},
		{ID: "auditor", OnKind: eventRuleKindReviewVerdict, WakeRole: "auditor", Scope: db.EventRuleScopeObserver, Enabled: true},
	} {
		if err := store.AddEventRule(context.Background(), rule); err != nil {
			t.Fatal(err)
		}
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, home: home, wake: wake}
	sink.evaluate(context.Background(), events.Event{
		Type:            events.EventJobFinished,
		Cause:           events.EventCauseReviewVerdict,
		JobID:           "review-42",
		Repo:            "owner/repo",
		WakeTargetRole:  "author",
		WakeTargetRoles: []string{"requester", "author"},
		PullRequest:     42,
		ReviewDecision:  "approved",
	})

	sort.Strings(wake.panes)
	if got, want := fmt.Sprint(wake.panes), "[w1:p1 w1:p3 w1:p4]"; got != want {
		t.Fatalf("woken panes = %s, want %s", got, want)
	}
}

func TestTruncateForWakeRuneSafe(t *testing.T) {
	// A multibyte run whose byte length exceeds the cap; a naive detail[:max] would
	// split a rune and emit invalid UTF-8 into the herdr prompt.
	long := strings.Repeat("é", 400) // 800 bytes, 400 runes
	out := truncateForWake(long, 320)
	if !utf8.ValidString(out) {
		t.Fatalf("truncated prompt is not valid UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("expected ellipsis on truncation, got %q", out)
	}
	// A short ASCII string is returned unchanged (no ellipsis).
	if got := truncateForWake("ok", 320); got != "ok" {
		t.Fatalf("short string altered: %q", got)
	}
	// An invalid byte EARLY in the string must not collapse the tail: we shave
	// only the partial rune at the cut boundary, not react to bytes elsewhere.
	bad := "\xff" + strings.Repeat("b", 400)
	if got := truncateForWake(bad, 320); len(got) < 300 {
		t.Fatalf("early invalid byte over-trimmed the detail to %d bytes: %q", len(got), got)
	}
}
