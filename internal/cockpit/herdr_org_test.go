package cockpit

import (
	"context"
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
{"pane_id":"w1:p1","label":"owner","agent_status":"working"},
{"pane_id":"w1:p2","label":"review","agent_status":"blocked"},
{"pane_id":"w1:p3","label":"done","agent_status":"done"},
{"pane_id":"w1:p4","label":"idle","agent_status":"idle"},
{"pane_id":"w1:p5","label":"future","agent_status":"paused"},
{"pane_id":"w1:p6","label":"duplicate","agent_status":"working"},
{"pane_id":"w1:p7","label":"duplicate","agent_status":"idle"},
{"pane_id":"w1:p8","label":"claude","agent_status":"working"},
{"pane_id":"w1:p9","label":" whitespace-label ","agent_status":"working"},
{"pane_id":"w1:p10","label":"whitespace-status","agent_status":" working "}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{
		{Name: "owner"}, {Name: "review"}, {Name: "done"}, {Name: "idle"}, {Name: "future"},
		{Name: "duplicate"}, {Name: "missing"}, {Name: "whitespace-label"}, {Name: "whitespace-status"},
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
	}
	for role, want := range wants {
		if got := snapshot.States[role].State; got != want {
			t.Errorf("state[%s] = %q, want %q (%+v)", role, got, want, snapshot.States[role])
		}
	}
	if _, inferred := snapshot.States["claude"]; inferred {
		t.Fatal("provider inferred a runtime label that was not a configured role")
	}
}

func TestHerdrOrgProviderSnapshotPaneBindings(t *testing.T) {
	run := func(_ context.Context, _ ...string) (string, error) {
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"pane_id":"w1:p1","label":"Gitmoot Idle","agent_status":"idle"},
{"pane_id":"w1:p2","label":"Gitmoot Working","agent_status":"working"},
{"pane_id":"w1:p3","label":"Gitmoot Blocked","agent_status":"blocked"},
{"pane_id":"w1:p4","label":"Gitmoot Done","agent_status":"done"},
{"pane_id":"w1:p5","label":"literal-pane","agent_status":"working"},
{"pane_id":"w1:p6","label":"duplicate-label","agent_status":"idle"},
{"pane_id":"w1:p7","label":"duplicate-label","agent_status":"blocked"},
{"pane_id":"","label":"Empty A","agent_status":"blocked"},
{"pane_id":"","label":"Empty B","agent_status":"idle"}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{
		{Name: "idle-role", Pane: "Gitmoot Idle"},
		{Name: "working-role", Pane: "Gitmoot Working"},
		{Name: "blocked-role", Pane: "Gitmoot Blocked"},
		{Name: "done-role", Pane: "Gitmoot Done"},
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
	var got []string
	run := func(ctx context.Context, args ...string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("Recycle runner context has no deadline")
		}
		got = append([]string(nil), args...)
		return "", nil
	}
	provider := newHerdrOrgProvider(run, []config.OrgRole{{Name: "owner"}}, time.Now)
	req := org.RecycleRequest{Role: "owner", Pane: "w1:p2", Kind: "codex", AgentName: "owner", BootPrompt: "role: owner\n\nhandoff: ship it"}
	if err := provider.Recycle(context.Background(), req); err != nil {
		t.Fatalf("Recycle() error = %v", err)
	}
	want := []string{"agent", "start", "owner", "--kind", "codex", "--pane", "w1:p2", "--timeout", "30000", "--", req.BootPrompt}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Recycle() args = %q, want %q", got, want)
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
