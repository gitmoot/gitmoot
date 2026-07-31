package cockpit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/org"
)

func TestHerdrOrgProviderSnapshotMapping(t *testing.T) {
	observed := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	run := func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "api snapshot" {
			t.Fatalf("args = %v", args)
		}
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
	{"pane_id":"w1:p1","label":"owner","agent":"claude","agent_status":"working","terminal_title":"✳ Owner task","terminal_title_stripped":"Owner task","turn":2,"last_completed_turn":{"turn":1,"turn_epoch":1785163074397066532,"completed_unix_ms":1785163510704}},
{"pane_id":"w1:p2","label":"review","agent_status":"blocked"},
{"pane_id":"w1:p3","label":"done","agent_status":"done"},
	{"pane_id":"w1:p4","label":"idle","agent":"codex","agent_status":"idle","terminal_title":"Idle task","turn":0},
{"pane_id":"w1:p5","label":"future","agent_status":"paused"},
{"pane_id":"w1:p6","label":"duplicate","agent_status":"working"},
{"pane_id":"w1:p7","label":"duplicate","agent_status":"idle"},
{"pane_id":"w1:p8","label":"claude","agent_status":"working"},
{"pane_id":"w1:p9","label":" whitespace-label ","agent_status":"working"},
{"pane_id":"w1:p10","label":"whitespace-status","agent_status":" working "},
{"pane_id":"w1:p11","label":"pending-working","agent_status":"working","input_pending":true},
{"pane_id":"w1:p12","label":"pending-idle","agent_status":"idle","input_pending":true},
{"pane_id":"w1:p13","label":"pending-unknown","agent_status":"unknown","input_pending":true}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{
		{Name: "owner", Pane: "owner"}, {Name: "review", Pane: "review"}, {Name: "done", Pane: "done"},
		{Name: "idle", Pane: "idle"}, {Name: "future", Pane: "future"}, {Name: "duplicate", Pane: "duplicate"},
		{Name: "missing", Pane: "missing"}, {Name: "whitespace-label", Pane: "whitespace-label"},
		{Name: "whitespace-status", Pane: "whitespace-status"}, {Name: "pending-working", Pane: "pending-working"},
		{Name: "pending-idle", Pane: "pending-idle"}, {Name: "pending-unknown", Pane: "pending-unknown"},
	}, func() time.Time { return observed })
	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.ProviderVersion != "0.7.5" || !snapshot.ObservedAt.Equal(observed) {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	wants := map[string]org.LifecycleState{
		"owner": org.StateWorking, "review": org.StateBlocked, "done": org.StateDone,
		"idle": org.StateIdle, "future": org.StateUnknown, "duplicate": org.StateUnknown,
		"missing": org.StateUnknown, "whitespace-label": org.StateUnknown, "whitespace-status": org.StateUnknown,
		"pending-working": org.StateInputPending, "pending-idle": org.StateInputPending, "pending-unknown": org.StateInputPending,
	}
	for role, want := range wants {
		if got := snapshot.States[role].State; got != want {
			t.Errorf("state[%s] = %q, want %q (%+v)", role, got, want, snapshot.States[role])
		}
	}
	if _, inferred := snapshot.States["claude"]; inferred {
		t.Fatal("provider inferred a runtime label that was not a configured role")
	}
	ownerSession, ok := snapshot.Sessions["owner"]
	if !ok || ownerSession.PaneID != "w1:p1" || ownerSession.Agent != "claude" ||
		ownerSession.TaskTitle != "Owner task" || ownerSession.CurrentTurn == nil || *ownerSession.CurrentTurn != 2 {
		t.Fatalf("owner session = %+v, present=%t", ownerSession, ok)
	}
	idleSession, ok := snapshot.Sessions["idle"]
	if !ok || idleSession.CurrentTurn == nil || *idleSession.CurrentTurn != 0 || idleSession.TaskTitle != "Idle task" {
		t.Fatalf("idle session = %+v, present=%t", idleSession, ok)
	}
	for _, role := range []string{"review", "done", "future", "duplicate", "missing"} {
		if session, present := snapshot.Sessions[role]; present {
			t.Errorf("session[%s] = %+v, want absent", role, session)
		}
	}
	ownerActivity := snapshot.States["owner"].Activity
	if ownerActivity == nil {
		t.Fatal("owner activity is absent")
	}
	if ownerActivity.Turn != 1 || ownerActivity.TurnEpoch != 1785163074397066532 ||
		!ownerActivity.CompletedAt.Equal(time.UnixMilli(1785163510704).UTC()) {
		t.Errorf("owner activity = %+v", ownerActivity)
	}
	for _, role := range []string{"review", "done", "idle", "future", "duplicate", "missing"} {
		if activity := snapshot.States[role].Activity; activity != nil {
			t.Errorf("activity[%s] = %+v, want absent", role, activity)
		}
	}
}

func TestHerdrOrgProviderPresenceWakePaneBindingParity(t *testing.T) {
	const panes = `[
{"pane_id":"w1:p1","label":"unique-label","agent_status":"working"},
{"pane_id":"w1:p2","label":"duplicate-label","agent_status":"idle"},
{"pane_id":"w1:p3","label":"duplicate-label","agent_status":"blocked"},
{"pane_id":"w1:p4","label":"literal-label","agent_status":"done"},
{"pane_id":"w1:p5","label":"empty-binding","agent_status":"working"}
]`
	run := func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "api snapshot":
			return `{"result":{"snapshot":{"version":"0.7.5","panes":` + panes + `}}}`, nil
		case "pane list":
			return `{"result":{"panes":` + panes + `}}`, nil
		default:
			t.Fatalf("unexpected Herdr args: %v", args)
			return "", nil
		}
	}
	client := herdrClient{run: run}
	resolveWake := func(binding string) (string, bool) {
		paneID, ok := config.ResolveRolePaneBinding(context.Background(), binding, func(_ context.Context, label string) (string, bool) {
			resolved, found, err := client.resolvePaneByLabel(context.Background(), label)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				return "", false
			}
			return resolved, true
		})
		return paneID, ok
	}
	tests := []struct {
		name       string
		binding    string
		wantPane   string
		wantState  org.LifecycleState
		wantDetail string
		wantOK     bool
	}{
		{name: "empty binding", binding: "", wantState: org.StateUnknown, wantDetail: "Herdr pane binding is unset"},
		{name: "binding matching one label", binding: "unique-label", wantPane: "w1:p1", wantState: org.StateWorking, wantOK: true},
		{name: "binding matching multiple labels", binding: "duplicate-label", wantState: org.StateUnknown},
		{name: "literal pane id", binding: "w1:p4", wantPane: "w1:p4", wantState: org.StateDone, wantOK: true},
		{name: "absent literal pane id", binding: "w9:p9", wantState: org.StateUnknown},
		{name: "binding matching nothing", binding: "missing-label", wantState: org.StateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := newHerdrOrgProvider(run, []config.OrgRole{{
				Name: "empty-binding", Pane: test.binding,
			}}, time.Now).Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wakePane, wakeOK := resolveWake(test.binding)
			if wakePane != test.wantPane || wakeOK != test.wantOK {
				t.Fatalf("wake resolution = (%q, %t), want (%q, %t)", wakePane, wakeOK, test.wantPane, test.wantOK)
			}
			if got := snapshot.States["empty-binding"].State; got != test.wantState {
				t.Fatalf("presence state = %q, want %q; wake resolution = (%q, %t)", got, test.wantState, wakePane, wakeOK)
			}
			if test.wantDetail != "" && snapshot.States["empty-binding"].Detail != test.wantDetail {
				t.Fatalf("presence detail = %q, want %q", snapshot.States["empty-binding"].Detail, test.wantDetail)
			}
			if (snapshot.States["empty-binding"].State != org.StateUnknown) != wakeOK {
				t.Fatalf("presence/wake parity: presence = %+v, wake resolution = (%q, %t)", snapshot.States["empty-binding"], wakePane, wakeOK)
			}
		})
	}
}

func TestHerdrOrgProviderSnapshotPaneBindings(t *testing.T) {
	run := func(_ context.Context, _ ...string) (string, error) {
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"Gitmoot Idle","agent_status":"idle","last_completed_turn":{"turn":7,"turn_epoch":99,"completed_unix_ms":1785163510704}},
{"pane_id":"w1:p2","label":"Gitmoot Working","agent_status":"working"},
{"pane_id":"w1:p3","label":"Gitmoot Blocked","agent_status":"blocked"},
{"pane_id":"w1:p4","label":"Gitmoot Done","agent_status":"done"},
{"pane_id":"w1:p5","label":"literal-pane","agent_status":"working"},
{"pane_id":"w1:p6","label":"duplicate-label","agent_status":"idle"},
{"pane_id":"w1:p7","label":"duplicate-label","agent_status":"blocked"},
{"pane_id":"","label":"Empty A","agent_status":"blocked"},
{"pane_id":"","label":"Empty B","agent_status":"idle"},
{"pane_id":"w1:p8","label":"Gitmoot Pending","agent_status":"working","input_pending":true}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{
		{Name: "idle-role", Pane: "Gitmoot Idle"},
		{Name: "working-role", Pane: "Gitmoot Working"},
		{Name: "blocked-role", Pane: "Gitmoot Blocked"},
		{Name: "done-role", Pane: "Gitmoot Done"},
		{Name: "pending-role", Pane: "Gitmoot Pending"},
		{Name: "literal-role", Pane: "w1:p5"},
		{Name: "missing-role", Pane: "missing-label"},
		{Name: "ambiguous-role", Pane: "duplicate-label"},
		// #1095 regression: a pane with an empty pane_id is not a resolvable
		// target (mirrors the wake resolver). It must NOT seed statusByPaneID[""]
		// and leak another empty-id pane's status; the bound role stays Unknown.
		{Name: "empty-id-role", Pane: "Empty A"},
	}, time.Now)

	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	wants := map[string]org.LifecycleState{
		"idle-role": org.StateIdle, "working-role": org.StateWorking,
		"blocked-role": org.StateBlocked, "done-role": org.StateDone,
		"pending-role": org.StateInputPending,
		"literal-role": org.StateWorking, "missing-role": org.StateUnknown,
		"ambiguous-role": org.StateUnknown, "empty-id-role": org.StateUnknown,
	}
	for role, want := range wants {
		if got := snapshot.States[role].State; got != want {
			t.Errorf("state[%s] = %q, want %q (%+v)", role, got, want, snapshot.States[role])
		}
	}
	if got := snapshot.States["missing-role"].Detail; got != `no Herdr pane bound as "missing-label"` {
		t.Errorf("missing detail = %q", got)
	}
	if got := snapshot.States["ambiguous-role"].Detail; got != `multiple Herdr panes labeled "duplicate-label"` {
		t.Errorf("ambiguous detail = %q", got)
	}
	if got := snapshot.States["empty-id-role"].Detail; got != `no Herdr pane bound as "Empty A"` {
		t.Errorf("empty-id detail = %q", got)
	}
	if activity := snapshot.States["idle-role"].Activity; activity == nil ||
		activity.Turn != 7 || activity.TurnEpoch != 99 ||
		!activity.CompletedAt.Equal(time.UnixMilli(1785163510704).UTC()) {
		t.Errorf("idle-role activity = %+v", activity)
	}
	for _, role := range []string{"working-role", "blocked-role", "done-role", "literal-role", "missing-role", "ambiguous-role", "empty-id-role"} {
		if activity := snapshot.States[role].Activity; activity != nil {
			t.Errorf("activity[%s] = %+v, want absent", role, activity)
		}
	}
}

func TestHerdrOrgProviderSnapshotAbsentTurnIsOmitted(t *testing.T) {
	run := func(_ context.Context, _ ...string) (string, error) {
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"owner","agent_status":"idle"}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{{Name: "owner", Pane: "owner"}}, time.Now)
	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	state := snapshot.States["owner"]
	if state.State != org.StateIdle || state.Detail != "" {
		t.Fatalf("owner state = %+v", state)
	}
	if state.Activity != nil {
		t.Fatalf("owner activity = %+v, want absent", state.Activity)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if got, want := string(encoded), `{"state":"idle"}`; got != want {
		t.Fatalf("encoded state = %s, want %s", got, want)
	}
}

func TestHerdrOrgProviderSnapshotPartialTurnIsAbsent(t *testing.T) {
	run := func(_ context.Context, _ ...string) (string, error) {
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"owner","agent_status":"working","last_completed_turn":{"turn":1,"turn_epoch":2}}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{{Name: "owner", Pane: "owner"}}, time.Now)
	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if activity := snapshot.States["owner"].Activity; activity != nil {
		t.Fatalf("owner activity = %+v, want absent for incomplete turn", activity)
	}
}

func TestHerdrOrgProviderSnapshotInvalidTurnValuesAreAbsent(t *testing.T) {
	tests := []struct {
		name string
		turn string
	}{
		{name: "zero turn", turn: `{"turn":0,"turn_epoch":2,"completed_unix_ms":3}`},
		{name: "zero turn epoch", turn: `{"turn":1,"turn_epoch":0,"completed_unix_ms":3}`},
		{name: "zero completion time", turn: `{"turn":1,"turn_epoch":2,"completed_unix_ms":0}`},
		{name: "all zero", turn: `{"turn":0,"turn_epoch":0,"completed_unix_ms":0}`},
		{name: "negative turn", turn: `{"turn":-1,"turn_epoch":2,"completed_unix_ms":3}`},
		{name: "negative turn epoch", turn: `{"turn":1,"turn_epoch":-2,"completed_unix_ms":3}`},
		{name: "negative completion time", turn: `{"turn":1,"turn_epoch":2,"completed_unix_ms":-3}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := func(_ context.Context, _ ...string) (string, error) {
				return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"owner","agent_status":"working","last_completed_turn":` + test.turn + `}
]}}}`, nil
			}
			provider := newHerdrOrgProvider(run, []config.OrgRole{{Name: "owner", Pane: "owner"}}, time.Now)
			snapshot, err := provider.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if activity := snapshot.States["owner"].Activity; activity != nil {
				t.Fatalf("owner activity = %+v, want absent for invalid turn", activity)
			}
		})
	}
}

func TestHerdrOrgProviderSnapshotOversizedUint64TurnEpochFailsClosedPerPane(t *testing.T) {
	run := func(_ context.Context, _ ...string) (string, error) {
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"oversized","agent_status":"working","last_completed_turn":{"turn":1,"turn_epoch":9223372036854775808,"completed_unix_ms":3}},
{"pane_id":"w1:p2","label":"valid","agent_status":"idle","last_completed_turn":{"turn":4,"turn_epoch":5,"completed_unix_ms":6}},
{"pane_id":"w1:p3","label":"absent","agent_status":"blocked"}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{
		{Name: "oversized", Pane: "oversized"}, {Name: "valid", Pane: "valid"}, {Name: "absent", Pane: "absent"},
	}, time.Now)
	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if state := snapshot.States["oversized"]; state.State != org.StateWorking || state.Activity != nil {
		t.Errorf("oversized state = %+v, want working with absent activity", state)
	}
	valid := snapshot.States["valid"]
	if valid.State != org.StateIdle || valid.Activity == nil ||
		valid.Activity.Turn != 4 || valid.Activity.TurnEpoch != 5 ||
		!valid.Activity.CompletedAt.Equal(time.UnixMilli(6).UTC()) {
		t.Errorf("valid state = %+v", valid)
	}
	if state := snapshot.States["absent"]; state.State != org.StateBlocked || state.Activity != nil {
		t.Errorf("absent state = %+v, want blocked with absent activity", state)
	}
}

func TestHerdrOrgProviderFailures(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{name: "command", err: errors.New("socket unavailable"), want: "herdr api snapshot"},
		{name: "json", out: `{`, want: "parse herdr api snapshot"},
		{name: "missing version", out: `{"result":{"snapshot":{"panes":[]}}}`, want: "incomplete snapshot"},
		{name: "missing panes", out: `{"result":{"snapshot":{"version":"0.7.5"}}}`, want: "incomplete snapshot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newHerdrOrgProvider(func(context.Context, ...string) (string, error) { return test.out, test.err }, []config.OrgRole{{Name: "owner"}}, time.Now)
			_, err := provider.Snapshot(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Snapshot() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestHerdrOrgProviderRecycleCommand(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  string
		model string
		want  []string
	}{
		{
			name: "unpinned preserves command",
			kind: "codex",
			want: []string{"agent", "start", "owner", "--kind", "codex", "--pane", "w1:p2", "--timeout", "30000", "--", "role: owner\n\nhandoff: ship it"},
		},
		{
			name:  "pinned model precedes boot prompt",
			kind:  "codex",
			model: "sonnet",
			want:  []string{"agent", "start", "owner", "--kind", "codex", "--pane", "w1:p2", "--timeout", "30000", "--", "--model", "sonnet", "role: owner\n\nhandoff: ship it"},
		},
		{
			name:  "pinned model trims stray whitespace",
			kind:  "claude",
			model: "  sonnet  ",
			want:  []string{"agent", "start", "owner", "--kind", "claude", "--pane", "w1:p2", "--timeout", "30000", "--", "--model", "sonnet", "role: owner\n\nhandoff: ship it"},
		},
		{
			name:  "pinned model applies for kimi",
			kind:  "kimi",
			model: "k2",
			want:  []string{"agent", "start", "owner", "--kind", "kimi", "--pane", "w1:p2", "--timeout", "30000", "--", "--model", "k2", "role: owner\n\nhandoff: ship it"},
		},
		{
			name:  "pinned model ignored for an unverified kind",
			kind:  "gemini",
			model: "sonnet",
			want:  []string{"agent", "start", "owner", "--kind", "gemini", "--pane", "w1:p2", "--timeout", "30000", "--", "role: owner\n\nhandoff: ship it"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			run := func(ctx context.Context, args ...string) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("Recycle runner context has no deadline")
				}
				got = append([]string(nil), args...)
				return "", nil
			}
			provider := newHerdrOrgProvider(run, []config.OrgRole{{Name: "owner"}}, time.Now)
			req := org.RecycleRequest{
				Role: "owner", Pane: "w1:p2", Kind: test.kind, AgentName: "owner",
				Model: test.model, BootPrompt: "role: owner\n\nhandoff: ship it",
			}
			if err := provider.Recycle(context.Background(), req); err != nil {
				t.Fatalf("Recycle() error = %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("Recycle() args = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHerdrOrgProviderRecycleFailureIsActionable(t *testing.T) {
	provider := newHerdrOrgProvider(func(context.Context, ...string) (string, error) {
		return "", errors.New("pane is not at shell")
	}, []config.OrgRole{{Name: "owner"}}, time.Now)
	err := provider.Recycle(context.Background(), org.RecycleRequest{Role: "owner", Pane: "w1:p2", Kind: "codex", AgentName: "owner", BootPrompt: "brief"})
	if err == nil || !strings.Contains(err.Error(), "interactive shell prompt") || !strings.Contains(err.Error(), "pane is not at shell") {
		t.Fatalf("Recycle() error = %v", err)
	}
}
