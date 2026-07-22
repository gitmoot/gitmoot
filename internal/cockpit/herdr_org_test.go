package cockpit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/org"
)

func TestHerdrOrgProviderSnapshotMapping(t *testing.T) {
	observed := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	run := func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "api snapshot" {
			t.Fatalf("args = %v", args)
		}
		return `{"result":{"snapshot":{"version":"0.7.5","panes":[
{"label":"owner","agent_status":"working"},
{"label":"review","agent_status":"blocked"},
{"label":"done","agent_status":"done"},
{"label":"idle","agent_status":"idle"},
{"label":"future","agent_status":"paused"},
{"label":"duplicate","agent_status":"working"},
{"label":"duplicate","agent_status":"idle"},
{"label":"claude","agent_status":"working"},
{"label":" whitespace-label ","agent_status":"working"},
{"label":"whitespace-status","agent_status":" working "}
]}}}`, nil
	}
	provider := newHerdrOrgProvider(run, []string{"owner", "review", "done", "idle", "future", "duplicate", "missing", "whitespace-label", "whitespace-status"}, func() time.Time { return observed })
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
			provider := newHerdrOrgProvider(func(context.Context, ...string) (string, error) { return test.out, test.err }, []string{"owner"}, time.Now)
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
	provider := newHerdrOrgProvider(run, []string{"owner"}, time.Now)
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
	}, []string{"owner"}, time.Now)
	err := provider.Recycle(context.Background(), org.RecycleRequest{Role: "owner", Pane: "w1:p2", Kind: "codex", AgentName: "owner", BootPrompt: "brief"})
	if err == nil || !strings.Contains(err.Error(), "interactive shell prompt") || !strings.Contains(err.Error(), "pane is not at shell") {
		t.Fatalf("Recycle() error = %v", err)
	}
}
