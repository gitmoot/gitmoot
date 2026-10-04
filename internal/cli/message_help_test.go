package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/org"
)

// messageHelpStoreCounts opens home's store and counts every message and every
// wake_outbox row, whatever its state.
func messageHelpStoreCounts(t *testing.T, home string) (int, int) {
	t.Helper()
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	messages, err := store.ListDashboardMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wakes, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	return len(messages), len(wakes)
}

// TestMessageHelpPrintsUsageAndSavesNothing covers #2306: a help flag on any
// message subcommand, in the place a user naturally types it, prints that
// subcommand's usage and never opens the store or saves mail.
func TestMessageHelpPrintsUsageAndSavesNothing(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("GITMOOT_ORG_ROLE", "")
	cases := []struct {
		name string
		args func(home, help string) []string
		want string
	}{
		{"send", func(home, help string) []string {
			return []string{"send", "jarvis", help, "--role", "deimos", "--home", home}
		}, "gitmoot message send ROLE"},
		{"reply", func(home, help string) []string {
			return []string{"reply", "1", help, "--role", "deimos", "--home", home}
		}, "gitmoot message reply ID"},
		{"inbox", func(home, help string) []string {
			return []string{"inbox", help, "--role", "deimos", "--home", home}
		}, "gitmoot message inbox"},
		{"show", func(home, help string) []string {
			return []string{"show", help, "--role", "deimos", "--home", home}
		}, "gitmoot message show ID"},
		{"escalate", func(home, help string) []string {
			return []string{"escalate", "--role", "deimos", "--home", home, help}
		}, "gitmoot message escalate"},
		{"resolve", func(home, help string) []string {
			return []string{"resolve", "1", "--role", "owner", "--home", home, help}
		}, "gitmoot message resolve ID"},
		{"pending", func(home, help string) []string {
			return []string{"pending", "--claim", "--hook", "Stop", "--runtime", "claude", "--role", "deimos", "--home", home, help}
		}, "gitmoot message pending"},
		{"directive", func(home, help string) []string {
			return []string{"directive", help}
		}, "gitmoot message directive send"},
		{"directive-send", func(home, help string) []string {
			return []string{"directive", "send", "--to", "deimos", "--role", "owner", "--home", home, help}
		}, "gitmoot message directive send"},
		{"directive-ack", func(home, help string) []string {
			return []string{"directive", "ack", "1", "--role", "deimos", "--home", home, help}
		}, "gitmoot message directive ack"},
		{"directive-done", func(home, help string) []string {
			return []string{"directive", "done", help, "--role", "deimos", "--home", home}
		}, "gitmoot message directive done"},
		{"directive-cancel", func(home, help string) []string {
			return []string{"directive", "cancel", "1", help, "--role", "owner", "--home", home}
		}, "gitmoot message directive cancel"},
	}
	for _, tc := range cases {
		for _, help := range []string{"-h", "-help", "--help"} {
			t.Run(tc.name+help, func(t *testing.T) {
				home := messageTestHome(t)
				var out, diag bytes.Buffer
				code := Run(append([]string{"message"}, tc.args(home, help)...), &out, &diag)
				_, statErr := os.Stat(config.PathsForHome(home).Database)
				messages, wakes := messageHelpStoreCounts(t, home)
				if messages != 0 || wakes != 0 {
					t.Fatalf("help saved %d messages and %d wake_outbox rows; stdout=%q stderr=%q", messages, wakes, out.String(), diag.String())
				}
				if code != 0 || !strings.Contains(out.String(), tc.want) {
					t.Fatalf("exit=%d, want 0 with usage %q on stdout; stdout=%q stderr=%q", code, tc.want, out.String(), diag.String())
				}
				if !os.IsNotExist(statErr) {
					t.Fatalf("help opened the store (database stat err=%v)", statErr)
				}
			})
		}
	}
}

// TestMessageRefusesFlagLikeText covers the other half of #2306: text that is
// a single flag-like word is a misplaced flag, not a message, so it is refused
// and nothing is saved. Text that merely starts with a dash is still sent.
func TestMessageRefusesFlagLikeText(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("GITMOOT_ORG_ROLE", "")
	home := messageTestHome(t)
	var sent, escalation messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "send", "jarvis", "- see PR 12 first"), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "- see PR 12 first" {
		t.Fatalf("dash-led text changed: %+v", sent)
	}
	var escalateOut, escalateDiag bytes.Buffer
	if code := Run([]string{"message", "escalate", "--role", "deimos", "--home", home, "--json", "Ship the release?"}, &escalateOut, &escalateDiag); code != 0 {
		t.Fatalf("escalate exit=%d: %s", code, escalateDiag.String())
	}
	if err := json.Unmarshal(escalateOut.Bytes(), &escalation); err != nil {
		t.Fatal(err)
	}
	beforeMessages, beforeWakes := messageHelpStoreCounts(t, home)
	for _, args := range [][]string{
		{"send", "jarvis", "--urgent", "--role", "deimos", "--home", home},
		{"send", "jarvis", "-x", "--role", "deimos", "--home", home},
		{"reply", fmt.Sprint(sent.ID), "--yes", "--role", "jarvis", "--home", home},
		{"escalate", "--role", "deimos", "--home", home, "--urgent"},
		{"resolve", fmt.Sprint(escalation.ID), "--answer", "--yes", "--role", "owner", "--home", home},
		{"directive", "send", "--to", "deimos", "--role", "owner", "--home", home, "-"},
	} {
		var out, diag bytes.Buffer
		code := Run(append([]string{"message"}, args...), &out, &diag)
		if code != 2 || !strings.Contains(diag.String(), "looks like a flag") {
			t.Errorf("%v: exit=%d, want 2 with flag-like refusal; stdout=%q stderr=%q", args, code, out.String(), diag.String())
		}
	}
	messages, wakes := messageHelpStoreCounts(t, home)
	if messages != beforeMessages || wakes != beforeWakes {
		t.Fatalf("flag-like text was saved: messages %d -> %d, wake_outbox rows %d -> %d", beforeMessages, messages, beforeWakes, wakes)
	}
	var explicit messageTestResult
	var out, diag bytes.Buffer
	if code := Run([]string{"message", "directive", "send", "--to", "deimos", "--role", "owner", "--home", home, "--json", "--", "--now"}, &out, &diag); code != 0 {
		t.Fatalf("directive body after -- refused: exit=%d stderr=%q", code, diag.String())
	}
	if err := json.Unmarshal(out.Bytes(), &explicit); err != nil || !strings.Contains(explicit.Body, "--now") {
		t.Fatalf("explicit directive body not saved: %+v err=%v", explicit, err)
	}
}

// TestOrgNotesRefuseHelpAndFlagLikeText applies the #2306 rules to org commands
// that journal free text: a help flag where the text belongs prints usage, and
// a flag-like handoff or reason is refused, so neither is saved.
func TestOrgNotesRefuseHelpAndFlagLikeText(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("GITMOOT_ORG_ROLE", "")
	original := newOrgProvider
	newOrgProvider = func([]config.OrgRole) org.Provider { return nil }
	t.Cleanup(func() { newOrgProvider = original })

	home := messageTestHome(t)
	var sent messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "send", "jarvis", "hello"), &sent); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	wakes, err := store.ListWakeOutbox(ctx, "")
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes=%+v err=%v", wakes, err)
	}
	wakeID := fmt.Sprint(wakes[0].ID)

	for _, tc := range []struct {
		args     []string
		wantCode int
		want     string
	}{
		{[]string{"recycle", "deimos", "--kind", "pi", "--pane", "p1", "--handoff", "--help"}, 0, "Usage: gitmoot org recycle"},
		{[]string{"recycle", "deimos", "--kind", "pi", "--pane", "p1", "--handoff", "-x"}, 2, "looks like a flag"},
		{[]string{"wake", "supersede", wakeID, "--reason", "--help"}, 0, "Usage: gitmoot org wake"},
		{[]string{"wake", "supersede", wakeID, "--reason", "-x"}, 2, "looks like a flag"},
	} {
		var out, diag bytes.Buffer
		code := Run(append(append([]string{"org"}, tc.args...), "--home", home), &out, &diag)
		if code != tc.wantCode || !strings.Contains(out.String()+diag.String(), tc.want) {
			t.Errorf("%v: exit=%d, want %d with %q; stdout=%q stderr=%q", tc.args, code, tc.wantCode, tc.want, out.String(), diag.String())
		}
	}
	notes, err := store.ListWorkflowNotes(ctx, "org/deimos", 10)
	if err != nil || len(notes) != 0 {
		t.Fatalf("recycle journaled a handoff: %+v err=%v", notes, err)
	}
	row, err := store.GetWakeOutbox(ctx, wakes[0].ID)
	if err != nil || row.State != db.WakeOutboxStatePending {
		t.Fatalf("wake was recovered: %+v err=%v", row, err)
	}
}
