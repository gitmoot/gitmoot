package cockpit

import (
	"context"
	"testing"
)

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
