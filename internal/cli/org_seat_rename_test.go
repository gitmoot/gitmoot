package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
)

// #2502: the owner renamed the Herdr agent behind seat enyo-worker to "tino".
// The binding "agent:enyo-worker" then resolved to nothing and every message
// to the seat queued silently for hours. The daemon's org pass must follow the
// same agent (same pane, same session) by rewriting the seat's binding and
// telling its lead; an agent in that pane with another session is never
// followed, and the lead is told once that the seat is unreachable.
func TestOrgSeatFollowsRenamedAgentBySameSession(t *testing.T) {
	const pane, session = "w1:pCK", "/root/.omp/agent/sessions/-lenscale/s1.jsonl"
	cases := []struct {
		name        string
		agents      []org.LiveAgent
		wantBinding string
		wantNotice  string
	}{
		{"same pane and session is followed", []org.LiveAgent{{Name: "tino", PaneID: pane, Session: session}}, "agent:tino", `renamed from "worker" to "tino"`},
		{"another session in that pane is not followed", []org.LiveAgent{{Name: "fresh", PaneID: pane, Session: "/root/.omp/agent/sessions/-lenscale/s2.jsonl"}}, "agent:worker", `seat worker is unreachable`},
		{"no agent left is not followed", nil, "agent:worker", `no agent runs there now`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("\n[org.roles.\"owner\"]\nscope = [\"*\"]\npane = \"agent:owner\"\n[org.roles.\"worker\"]\nparent = \"owner\"\nscope = [\"*\"]\npane = \"agent:worker\"\n"); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
			store, err := dbtest.Open(t, paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			now := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
			bound := org.Snapshot{
				States: map[string]org.RoleLiveState{"owner": {State: org.StateIdle}, "worker": {State: org.StateIdle}},
				PaneBindings: map[string]org.PaneBinding{
					"owner":  {PaneID: "w1:pL", AgentName: "owner", Session: "/s/owner.jsonl"},
					"worker": {PaneID: pane, AgentName: "worker", Session: session},
				},
				Agents:     []org.LiveAgent{{Name: "owner", PaneID: "w1:pL", Session: "/s/owner.jsonl"}, {Name: "worker", PaneID: pane, Session: session}},
				ObservedAt: now, ProviderVersion: "test-v1",
			}
			renamed := bound
			renamed.States = map[string]org.RoleLiveState{"owner": {State: org.StateIdle}, "worker": {State: org.StateUnknown}}
			renamed.PaneBindings = map[string]org.PaneBinding{"owner": bound.PaneBindings["owner"], "worker": {Detail: `no Herdr pane bound as "agent:worker"`}}
			renamed.Agents = append([]org.LiveAgent{{Name: "owner", PaneID: "w1:pL", Session: "/s/owner.jsonl"}}, tc.agents...)

			tick := func(snapshot org.Snapshot, at time.Time) {
				runBlockedRoleWakeOnce(ctx, store, home, io.Discard, at, blockedRoleWakeDependencies{
					availability: &fakeBlockedRoleAvailability{available: true},
					provider:     func([]config.OrgRole) org.Provider { return orgFixtureProvider{snapshot: snapshot} },
					eventSink:    func(context.Context, *db.Store, string) (events.Sink, error) { return nil, nil },
					archiveList: func(context.Context) ([]byte, error) {
						return []byte(`{"result":{"agents":[]}}`), nil
					},
				})
			}
			// The provider reads the binding fresh each tick, so once the seat
			// was rebound to "tino" the next snapshot resolves it there.
			after := renamed
			if tc.wantBinding == "agent:tino" {
				after.States = bound.States
				after.PaneBindings = map[string]org.PaneBinding{"owner": bound.PaneBindings["owner"], "worker": {PaneID: pane, AgentName: "tino", Session: session}}
			}
			tick(bound, now)
			tick(renamed, now.Add(time.Minute))
			tick(after, now.Add(2*time.Minute))

			cfg, err := config.LoadOrg(paths)
			if err != nil {
				t.Fatal(err)
			}
			if role, _ := cfg.Role("worker"); role.Pane != tc.wantBinding {
				t.Fatalf("worker binding = %q, want %q", role.Pane, tc.wantBinding)
			}
			messages, err := store.ListMessages(ctx, "owner", 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			notices := 0
			for _, message := range messages {
				if strings.Contains(message.Body, "seat worker") || strings.Contains(message.Body, "of seat worker") {
					notices++
					if !strings.Contains(message.Body, tc.wantNotice) {
						t.Fatalf("lead notice = %q, want %q", message.Body, tc.wantNotice)
					}
				}
			}
			if notices != 1 {
				t.Fatalf("lead got %d notices about worker over two unresolved ticks, want exactly 1: %+v", notices, messages)
			}
		})
	}
}
