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
// to the seat queued silently for hours. Each case runs the daemon's real org
// pass over a sequence of Herdr snapshots and checks the seat's binding in the
// org config and the notices its parent ("owner") received.
func TestOrgSeatFollowsRenamedAgentBySameSession(t *testing.T) {
	const pane, session = "w1:pCK", "/root/.omp/agent/sessions/-lenscale/s1.jsonl"
	resolvedAs := func(agent, sess string) seatTick {
		return seatTick{binding: org.PaneBinding{PaneID: pane, AgentName: agent, Session: sess}, agents: []org.LiveAgent{{Name: agent, PaneID: pane, Session: sess}}}
	}
	unresolved := func(agents ...org.LiveAgent) seatTick {
		return seatTick{binding: org.PaneBinding{Detail: "no Herdr pane bound"}, agents: agents}
	}
	renamed := org.LiveAgent{Name: "tino", PaneID: pane, Session: session}
	cases := []struct {
		name        string
		ticks       []seatTick
		wantBinding string
		wantNotices []string
	}{
		{"same pane and session is followed",
			[]seatTick{resolvedAs("worker", session), unresolved(renamed), resolvedAs("tino", session)},
			"agent:tino", []string{`renamed from "worker" to "tino"`}},
		{"another session in that pane is not followed",
			[]seatTick{resolvedAs("worker", session), unresolved(org.LiveAgent{Name: "fresh", PaneID: pane, Session: "/s2.jsonl"}), unresolved(org.LiveAgent{Name: "fresh", PaneID: pane, Session: "/s2.jsonl"})},
			"agent:worker", []string{`agent "fresh" runs there now, not provably the same agent`}},
		{"no agent left is not followed",
			[]seatTick{resolvedAs("worker", session), unresolved(), unresolved()},
			"agent:worker", []string{`no agent runs there now`}},
		// #2503 review: a seat already unreachable when gitmoot first sees it
		// has no recorded identity; it is still reported, once.
		{"already unreachable with no record is reported once",
			[]seatTick{unresolved(renamed), unresolved(renamed)},
			"agent:worker", []string{`gitmoot has no record of where it last ran`}},
		// #2503 review: a resolution WITHOUT a session ends the alert episode
		// and replaces the old proof, so a later rename is not followed and
		// the new outage is reported again.
		{"resolution without session ends the episode and the proof",
			[]seatTick{resolvedAs("worker", session), unresolved(), resolvedAs("worker", ""), unresolved(renamed)},
			"agent:worker", []string{`no agent runs there now`, `agent "tino" runs there now, not provably the same agent`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home, store, paths := orgSeatRenameStore(t)
			now := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
			for i, tick := range tc.ticks {
				runOrgSeatTick(ctx, t, store, home, tick, now.Add(time.Duration(i)*time.Minute))
			}
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
			var got []string
			for i := len(messages) - 1; i >= 0; i-- {
				if strings.Contains(messages[i].Body, "seat worker") {
					got = append(got, messages[i].Body)
				}
			}
			if len(got) != len(tc.wantNotices) {
				t.Fatalf("owner got %d notices about worker, want %d: %q", len(got), len(tc.wantNotices), got)
			}
			for i, want := range tc.wantNotices {
				if !strings.Contains(got[i], want) {
					t.Fatalf("notice %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

type seatTick struct {
	binding org.PaneBinding
	agents  []org.LiveAgent
}

func orgSeatRenameStore(t *testing.T) (string, *db.Store, config.Paths) {
	t.Helper()
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
	t.Cleanup(func() { _ = store.Close() })
	return home, store, paths
}

func runOrgSeatTick(ctx context.Context, t *testing.T, store *db.Store, home string, tick seatTick, at time.Time) {
	t.Helper()
	owner := org.LiveAgent{Name: "owner", PaneID: "w1:pL", Session: "/s/owner.jsonl"}
	workerState := org.StateIdle
	if tick.binding.PaneID == "" {
		workerState = org.StateUnknown
	}
	snapshot := org.Snapshot{
		States: map[string]org.RoleLiveState{"owner": {State: org.StateIdle}, "worker": {State: workerState}},
		PaneBindings: map[string]org.PaneBinding{
			"owner":  {PaneID: owner.PaneID, AgentName: owner.Name, Session: owner.Session},
			"worker": tick.binding,
		},
		Agents:     append([]org.LiveAgent{owner}, tick.agents...),
		ObservedAt: at, ProviderVersion: "test-v1",
	}
	runBlockedRoleWakeOnce(ctx, store, home, io.Discard, at, blockedRoleWakeDependencies{
		availability: &fakeBlockedRoleAvailability{available: true},
		provider:     func([]config.OrgRole) org.Provider { return orgFixtureProvider{snapshot: snapshot} },
		eventSink:    func(context.Context, *db.Store, string) (events.Sink, error) { return nil, nil },
		archiveList: func(context.Context) ([]byte, error) {
			return []byte(`{"result":{"agents":[]}}`), nil
		},
	})
}
