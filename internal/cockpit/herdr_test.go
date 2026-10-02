package cockpit

import (
	"context"
	"errors"
	"testing"
)

func TestHerdrAgentPromptRequiresSubmissionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, response       string
		transportErr         error
		delivered, uncertain bool
	}{
		{"submitted", `{"result":{"type":"agent_prompted","delivery":"submitted"}}`, nil, true, false},
		{"written only", `{"result":{"type":"agent_prompted","delivery":"written_to_pty"}}`, nil, false, true},
		{"legacy success lacks receipt", `{"result":{"type":"agent_prompted"}}`, nil, false, true},
		{"timeout before write", `{"error":{"code":"timeout","message":"deadline before PTY write"}}`, errors.New("exit 1"), false, true},
		{"submitted but not settled", `{"error":{"code":"agent_status_unobserved_after_submit"}}`, errors.New("exit 1"), true, false},
		{"legacy stall", `{"error":{"code":"agent_prompt_stalled"}}`, errors.New("exit 1"), false, true},
		{"visible draft", `{"error":{"code":"agent_prompt_unsubmitted"}}`, errors.New("exit 1"), false, true},
		{"receipt lost", `{"result":`, errors.New("killed"), false, true},
		{"agent absent", `{"error":{"code":"agent_not_found"}}`, errors.New("exit 1"), false, false},
		{"modal rejected input", `{"error":{"code":"agent_input_pending"}}`, errors.New("exit 1"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := herdrClient{run: func(context.Context, ...string) (string, error) { return tc.response, tc.transportErr }}
			delivered, uncertain, err := client.agentPrompt(context.Background(), "w1:p2", "review this", "")
			if delivered != tc.delivered || uncertain != tc.uncertain || (delivered && err != nil) || (!delivered && err == nil) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want delivered=%v uncertain=%v with diagnostic on non-delivery", delivered, uncertain, err, tc.delivered, tc.uncertain)
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
