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

const (
	studioID = "8195b6326f748f4da1945364a4e205b9"
	h2ID     = "9b0df6f6a46cc3e557f6840a1cde0a67"
)

// remoteHerdr answers the three Herdr calls the org provider makes: a local
// api snapshot, the federated agent list and the saved machines.
func remoteHerdr(t *testing.T, agents string, calls *[]string) runner {
	t.Helper()
	return func(_ context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if calls != nil {
			*calls = append(*calls, joined)
		}
		switch joined {
		case "api snapshot":
			return `{"result":{"snapshot":{"version":"0.9.5","panes":[{"pane_id":"w1:p1","label":"local","agent":"omp","agent_status":"working"}]}}}`, nil
		case "agent list":
			return `{"result":{"agents":[{"name":"local-seat","pane_id":"w1:p1","agent":"omp"},` + agents + `]}}`, nil
		case "machine list --json":
			return `[{"id":"` + studioID + `","label":"jerrys-mac-studio"},{"id":"` + h2ID + `","label":"hetzner2"}]`, nil
		}
		t.Fatalf("unexpected herdr call %q", joined)
		return "", nil
	}
}

func remoteAgent(machine, name, status, reachability string) string {
	return `{"name":"` + machine + `/` + name + `","pane_id":"` + machine + `/w1:p9","agent":"omp","machine_id":"` + machine +
		`","machine_profile_id":"` + machine + `","agent_status":"` + status + `","reachability":"` + reachability +
		`","turn":6,"terminal_title_stripped":"working on macserve","agent_session":{"value":"sess-1"},` +
		`"last_completed_turn":{"turn":6,"turn_epoch":42,"completed_unix_ms":1795120827698}}`
}

func TestRemoteBinding(t *testing.T) {
	cases := map[string]bool{
		"agent:jerrys-mac-studio/mac-studio": true,
		"agent:" + studioID + "/mac-studio":  true,
		" agent:hetzner2/gm-remote-test ":    true,
		"agent:mac-studio":                   false, // local agent binding
		"agent:/mac-studio":                  false,
		"agent:hetzner2/":                    false,
		"agent:a/b/c":                        false,
		"w1:p9":                              false,
		"jerrys-mac-studio/mac-studio":       false, // only agent: bindings name agents
	}
	for binding, want := range cases {
		if _, _, got := RemoteBinding(binding); got != want {
			t.Errorf("RemoteBinding(%q) = %v, want %v", binding, got, want)
		}
	}
}

func TestRemoteAgentForBindingFailsClosed(t *testing.T) {
	machines := []savedMachine{{ID: studioID, Label: "jerrys-mac-studio"}, {ID: h2ID, Label: "hetzner2"}}
	agent := func(machine, name string) registeredAgent {
		return registeredAgent{Name: machine + "/" + name, PaneID: machine + "/w1:p9", MachineID: machine}
	}
	live := []registeredAgent{
		agent(studioID, "mac-studio"),
		agent(h2ID, "twin"), agent(h2ID, "twin"),
		{Name: "mac-studio", PaneID: "w1:p2"}, // a local agent with the same name
		func() registeredAgent { a := agent(h2ID, "gone"); a.Archived = []byte(`"2026-11-19"`); return a }(),
	}
	cases := []struct {
		binding, wantPane string
	}{
		{"agent:jerrys-mac-studio/mac-studio", studioID + "/w1:p9"},
		{"agent:" + studioID + "/mac-studio", studioID + "/w1:p9"},
		{"agent:hetzner2/mac-studio", ""},        // name lives on another machine
		{"agent:unknown-machine/mac-studio", ""}, // not a saved machine
		{"agent:hetzner2/twin", ""},              // two agents, ambiguous
		{"agent:hetzner2/gone", ""},              // archived
		{"agent:hetzner2/missing", ""},
	}
	for _, tc := range cases {
		got, ok := remoteAgentForBinding(live, machines, tc.binding)
		if tc.wantPane == "" {
			if ok {
				t.Errorf("%s resolved to %+v, want no resolution", tc.binding, got)
			}
			continue
		}
		if !ok || got.PaneID != tc.wantPane {
			t.Errorf("%s = %+v, %v; want pane %s", tc.binding, got, ok, tc.wantPane)
		}
	}
	// A label two saved machines share names neither, even when both have the agent.
	twinMachines := append(machines, savedMachine{ID: "other", Label: "jerrys-mac-studio"})
	withTwin := append(append([]registeredAgent{}, live...), agent("other", "mac-studio"))
	if got, ok := remoteAgentForBinding(withTwin, twinMachines, "agent:jerrys-mac-studio/mac-studio"); ok {
		t.Errorf("ambiguous machine label resolved to %+v", got)
	}
	// The machine id Herdr reports wins over the name: an entry named for the
	// studio but reported from another machine is not the studio's agent.
	spoofed := agent(studioID, "mac-studio")
	spoofed.MachineID = h2ID
	if got, ok := remoteAgentForBinding([]registeredAgent{spoofed}, machines, "agent:jerrys-mac-studio/mac-studio"); ok {
		t.Errorf("agent reported from %s resolved for the studio: %+v", h2ID, got)
	}
	// An unqualified agent binding never reaches a remote agent of that name.
	if got, ok := registeredAgentForBinding([]registeredAgent{agent(studioID, "mac-studio")}, "agent:mac-studio"); ok {
		t.Errorf("agent:mac-studio resolved to remote %+v", got)
	}
}

func TestHerdrOrgProviderRemoteRole(t *testing.T) {
	agents := strings.Join([]string{
		remoteAgent(studioID, "mac-studio", "idle", "reachable"),
		remoteAgent(h2ID, "away", "idle", "unreachable"),
	}, ",")
	provider := newHerdrOrgProvider(remoteHerdr(t, agents, nil), []config.OrgRole{
		{Name: "mac-studio", Pane: "agent:jerrys-mac-studio/mac-studio"},
		{Name: "away", Pane: "agent:hetzner2/away"},
		{Name: "missing", Pane: "agent:hetzner2/nobody"},
		{Name: "local", Pane: "agent:local-seat"},
	}, time.Now)
	snapshot, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	if got := snapshot.States["mac-studio"]; got.State != org.StateIdle || got.Activity == nil || got.Activity.Turn != 6 {
		t.Errorf("mac-studio state = %+v, want idle with turn 6", got)
	}
	if got := snapshot.PaneBindings["mac-studio"]; got.PaneID != studioID+"/w1:p9" || got.AgentName != studioID+"/mac-studio" || got.Session != "sess-1" {
		t.Errorf("mac-studio binding = %+v", got)
	}
	if got := snapshot.Sessions["mac-studio"]; got.Agent != "omp" || got.TaskTitle != "working on macserve" || got.CurrentTurn == nil || *got.CurrentTurn != 6 {
		t.Errorf("mac-studio session = %+v", got)
	}

	if got := snapshot.States["away"]; got.State != org.StateUnknown || !strings.Contains(got.Detail, `"unreachable"`) {
		t.Errorf("away state = %+v, want unknown naming the unreachable machine", got)
	}
	if got := snapshot.PaneBindings["away"]; got.PaneID != "" || !got.Unavailable {
		t.Errorf("away binding = %+v, want unresolved and unavailable", got)
	}
	if got := snapshot.States["missing"]; got.State != org.StateUnknown || snapshot.PaneBindings["missing"].PaneID != "" {
		t.Errorf("missing = %+v / %+v, want unknown and unresolved", got, snapshot.PaneBindings["missing"])
	}
	// A local role is resolved exactly as before.
	if got := snapshot.States["local"]; got.State != org.StateWorking || snapshot.PaneBindings["local"].PaneID != "w1:p1" {
		t.Errorf("local = %+v / %+v", got, snapshot.PaneBindings["local"])
	}
}

func TestHerdrOrgProviderAsksForMachinesOnlyWithARemoteRole(t *testing.T) {
	var calls []string
	provider := newHerdrOrgProvider(remoteHerdr(t, remoteAgent(studioID, "mac-studio", "idle", "reachable"), &calls),
		[]config.OrgRole{{Name: "local", Pane: "agent:local-seat"}}, time.Now)
	if _, err := provider.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	for _, call := range calls {
		if call == "machine list --json" {
			t.Errorf("machine list called without a remote role: %v", calls)
		}
	}
}

func TestAgentNotifyNeverReachesARemotePane(t *testing.T) {
	client := herdrClient{run: func(_ context.Context, args ...string) (string, error) {
		t.Fatalf("remote delivery ran herdr %q", strings.Join(args, " "))
		return "", nil
	}}
	target := NotificationTarget{Selector: studioID + "/mac-studio", PaneID: studioID + "/w1:p9", Kind: "omp", Machine: studioID}
	delivered, uncertain, err := client.agentNotify(context.Background(), t.TempDir(), target, "hello")
	var deferred *NotificationDeferred
	if delivered || uncertain || !errors.As(err, &deferred) || deferred.Reason != NotificationRemotePending {
		t.Fatalf("agentNotify = %v, %v, %v; want deferred with %q", delivered, uncertain, err, NotificationRemotePending)
	}
}
