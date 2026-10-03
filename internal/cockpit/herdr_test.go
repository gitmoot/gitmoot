package cockpit

import (
	"context"
	"errors"
	"testing"
)

func TestHerdrAgentNotifyRequiresAdmissionReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, response       string
		transportErr         error
		delivered, uncertain bool
	}{
		{"accepted", `{"result":{"type":"agent_prompt_safe","outcome":{"status":"accepted"}}}`, nil, true, false},
		{"draft", `{"result":{"type":"agent_prompt_safe","outcome":{"status":"deferred","reason":"draft"}}}`, nil, false, false},
		{"busy", `{"result":{"type":"agent_prompt_safe","outcome":{"status":"deferred","reason":"busy"}}}`, nil, false, false},
		{"legacy submission", `{"result":{"type":"agent_prompted","delivery":"submitted"}}`, nil, false, true},
		{"PTY write", `{"result":{"type":"agent_prompted","delivery":"written_to_pty"}}`, nil, false, true},
		{"timeout", `{"error":{"code":"timeout"}}`, errors.New("exit 1"), false, true},
		{"receipt lost", `{"result":`, errors.New("killed"), false, true},
		{"agent absent", `{"error":{"code":"agent_not_found"}}`, errors.New("exit 1"), false, false},
		{"unsupported server", `{"error":{"code":"method_not_found"}}`, errors.New("exit 1"), false, false},
		{"unknown outcome", `{"result":{"type":"agent_prompt_safe","outcome":{"status":"written"}}}`, nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := herdrClient{run: func(context.Context, ...string) (string, error) { return tc.response, tc.transportErr }}
			target := NotificationTarget{Selector: "worker", Runtime: &NotificationRuntime{RuntimeID: "runtime", SessionID: "session"}}
			delivered, uncertain, err := client.agentNotify(context.Background(), target, "review this")
			if delivered != tc.delivered || uncertain != tc.uncertain || (delivered && err != nil) || (!delivered && err == nil) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want delivered=%v uncertain=%v with diagnostic on non-delivery", delivered, uncertain, err, tc.delivered, tc.uncertain)
			}
			var deferred *NotificationDeferred
			if errors.As(err, &deferred) != (!delivered && !uncertain) {
				t.Fatalf("retryable refusal=%v; delivered=%v uncertain=%v err=%v", deferred, delivered, uncertain, err)
			}
		})
	}
}

func TestHerdrRegisteredSeatFollowsReplacementNotStaleLabel(t *testing.T) {
	agents := `[{"name":"worker","pane_id":"w1:new"}]`
	client := herdrClient{run: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "agent" {
			return `{"result":{"agents":` + agents + `}}`, nil
		}
		return `{"result":{"panes":[{"pane_id":"w1:old","label":"worker"},{"pane_id":"w1:new"}]}}`, nil
	}}
	for _, binding := range []string{"worker", "w1:old"} {
		if pane, ok, err := client.resolvePaneByLabel(context.Background(), binding); err != nil || ok || pane != "" {
			t.Fatalf("stale %q resolved to %q, %v, %v", binding, pane, ok, err)
		}
	}
	pane, ok, err := client.resolvePaneByLabel(context.Background(), "agent:worker")
	if err != nil || !ok || pane != "w1:new" {
		t.Fatalf("current seat = %q %v %v", pane, ok, err)
	}
	agents = `[{"name":"worker","pane_id":"w1:new"},{"name":"worker","pane_id":"w1:other"}]`
	if _, ok, _ := client.resolvePaneByLabel(context.Background(), "agent:worker"); ok {
		t.Fatal("ambiguous name resolved")
	}
	agents = `[{"name":"worker","pane_id":"peer/w1:new","machine_id":"peer"}]`
	if _, ok, _ := client.resolvePaneByLabel(context.Background(), "agent:worker"); ok {
		t.Fatal("local binding resolved to a remote agent")
	}
}
