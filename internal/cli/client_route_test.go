package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
)

// clientTestHome registers machine NAME for role on the hub home and returns a
// separate client home whose [client] points at a running hub.
func clientTestHome(t *testing.T, hubHome, name, role, agent string) (string, string) {
	t.Helper()
	certs := filepath.Join(t.TempDir(), "certs")
	if code, _, diag := hubCLI(t, hubHome, "machine", "add", name, "--role", role, "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	base := hubTestServer(t, hubHome)
	clientHome := t.TempDir()
	paths := config.PathsForHome(clientHome)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[client]\nhub = %q\ncerts = %q\nname = %q\n", base, certs, name)
	if agent != "" {
		body += fmt.Sprintf("agent = %q\n", agent)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return clientHome, base
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(args, &out, &diag)
	return code, out.String(), diag.String()
}

// The ordinary commands on a client home talk to the hub: a message sent from
// the client arrives on the hub, a reply on the hub reaches the client's
// inbox, and the org brief is the hub's.
func TestClientHomeCommandsUseTheHub(t *testing.T) {
	t.Setenv("GITMOOT_ORG_ROLE", "")
	hubHome := messageTestHome(t)
	clientHome, _ := clientTestHome(t, hubHome, "mac", "gm-omp-nag", "")

	code, out, diag := runCLI(t, "message", "send", "deimos", "sent from the mac", "--home", clientHome, "--json")
	if code != 0 {
		t.Fatalf("client send exit=%d: %s", code, diag)
	}
	var sent messageTestResult
	if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.Sender != "gm-omp-nag" || sent.Recipient != "deimos" {
		t.Fatalf("client send: %+v %v", sent, err)
	}
	var hubInbox []messageTestResult
	if err := json.Unmarshal(messageCLI(t, hubHome, "deimos", "inbox"), &hubInbox); err != nil || len(hubInbox) != 1 || hubInbox[0].ID != sent.ID {
		t.Fatalf("hub inbox = %+v %v", hubInbox, err)
	}
	messageCLI(t, hubHome, "deimos", "reply", fmt.Sprint(sent.ID), "answered on the hub")

	code, out, diag = runCLI(t, "message", "inbox", "--home", clientHome)
	if code != 0 || !strings.Contains(out, "answered on the hub") {
		t.Fatalf("client inbox exit=%d out=%s diag=%s", code, out, diag)
	}
	code, out, diag = runCLI(t, "org", "brief", "--role", "gm-omp-nag", "--home", clientHome, "--json")
	if code != 0 || !strings.Contains(out, `"gm-omp-nag"`) {
		t.Fatalf("client brief exit=%d out=%s diag=%s", code, out, diag)
	}
	if _, err := os.Stat(config.PathsForHome(clientHome).Database); !os.IsNotExist(err) {
		t.Fatalf("the client home grew a local database: %v", err)
	}
}

// A command the hub does not serve refuses on a client home instead of
// answering from an empty local database.
func TestClientHomeRefusesLocalOnlyCommands(t *testing.T) {
	hubHome := messageTestHome(t)
	clientHome, _ := clientTestHome(t, hubHome, "mac", "gm-omp-nag", "")
	code, _, diag := runCLI(t, "pr", "list", "--home", clientHome)
	if code == 0 || !strings.Contains(diag, "hub client") {
		t.Fatalf("pr list on a client home: exit=%d %s", code, diag)
	}
	if _, err := os.Stat(config.PathsForHome(clientHome).Database); !os.IsNotExist(err) {
		t.Fatalf("a refused command created a local database: %v", err)
	}
	code, _, diag = runCLI(t, "review", "request", "--repo", "gitmoot/implementation", "--pr", "7", "--head-sha", strings.Repeat("a", 40),
		"--role", "gm-omp-nag", "--evidence-manifest", "/etc/hostname", "--home", clientHome)
	if code == 0 || !strings.Contains(diag, "not available through the hub: --evidence-manifest") {
		t.Fatalf("host-path flag on a client: exit=%d %s", code, diag)
	}
}

type fakeClientNotifier struct {
	found     bool
	delivered bool
	deferred  string
	prompts   []string
}

func (f *fakeClientNotifier) ResolveNotificationTarget(context.Context, string) (cockpit.NotificationTarget, bool) {
	return cockpit.NotificationTarget{Kind: "omp"}, f.found
}

func (f *fakeClientNotifier) AgentNotify(_ context.Context, _ cockpit.NotificationTarget, prompt string) (bool, bool, error) {
	f.prompts = append(f.prompts, prompt)
	if f.deferred != "" {
		return false, false, &cockpit.NotificationDeferred{Reason: f.deferred}
	}
	return f.delivered, !f.delivered, nil
}

// The omp loop hands each claimed message to the local add-on and reports the
// outcome the add-on gave: refused goes back to pending, admitted is
// delivered with this machine's receipt.
func TestClientDeliverOnceReportsTheAddOnsOutcome(t *testing.T) {
	hubHome := messageTestHome(t)
	first := sendTestMessage(t, hubHome, "gm-omp-nag", "deimos", "Check the release.")
	clientHome, _ := clientTestHome(t, hubHome, "mac", "deimos", "mac-studio")
	client, cfg, ok, err := hubClientFor(clientHome)
	if err != nil || !ok {
		t.Fatalf("hub client: ok=%t %v", ok, err)
	}
	defer client.Close()
	var out bytes.Buffer

	refusing := &fakeClientNotifier{found: true, deferred: "omp inbox add-on not listening"}
	if err := clientDeliverOnce(context.Background(), client, refusing, cfg, &out); err != nil {
		t.Fatal(err)
	}
	if len(refusing.prompts) != 1 || !strings.Contains(refusing.prompts[0], fmt.Sprintf("gitmoot message show %d", first)) {
		t.Fatalf("prompts = %q", refusing.prompts)
	}
	if got := turnHookWakeReceipts(t, hubHome, "deimos")[fmt.Sprint(first)]; !strings.HasPrefix(got, "pending|") {
		t.Fatalf("after a refusal: %q, want pending", got)
	}

	admitting := &fakeClientNotifier{found: true, delivered: true}
	if err := clientDeliverOnce(context.Background(), client, admitting, cfg, &out); err != nil {
		t.Fatal(err)
	}
	if got := turnHookWakeReceipts(t, hubHome, "deimos")[fmt.Sprint(first)]; got != "delivered|omp-client:mac" {
		t.Fatalf("after admission: %q", got)
	}
	if err := clientDeliverOnce(context.Background(), client, admitting, cfg, &out); err != nil || len(admitting.prompts) != 1 {
		t.Fatalf("a delivered message was sent again: %d prompts, %v", len(admitting.prompts), err)
	}

	// Unknown outcome: the add-on may have shown it, so it is never resent.
	second := sendTestMessage(t, hubHome, "gm-omp-nag", "deimos", "Second: confirm the deploy window.")
	unsure := &fakeClientNotifier{found: true}
	if err := clientDeliverOnce(context.Background(), client, unsure, cfg, &out); err != nil || len(unsure.prompts) != 1 {
		t.Fatalf("uncertain pass: %d prompts, %v", len(unsure.prompts), err)
	}
	if got := turnHookWakeReceipts(t, hubHome, "deimos")[fmt.Sprint(second)]; !strings.HasPrefix(got, "attempted|") {
		t.Fatalf("after an unknown outcome: %q, want attempted", got)
	}
	if err := clientDeliverOnce(context.Background(), client, admitting, cfg, &out); err != nil || len(admitting.prompts) != 1 {
		t.Fatalf("an unknown-outcome message was resent: %d prompts, %v", len(admitting.prompts), err)
	}
}

// A client takes its role from the hub (the machine's only role) when no
// local role signal exists: the turn hook, org brief and review request all
// reach the hub instead of refusing locally (#2717 review).
func TestClientRoleDefaultsReachTheHub(t *testing.T) {
	t.Setenv("GITMOOT_ORG_ROLE", "")
	t.Setenv("HERDR_PANE_ID", "")
	hubHome := messageTestHome(t)
	sendTestMessage(t, hubHome, "gm-omp-nag", "deimos", "Mail for the remote seat.")
	clientHome, _ := clientTestHome(t, hubHome, "mac", "deimos", "")

	code, out, diag := runCLI(t, "org", "brief", "--home", clientHome, "--json")
	if code != 0 || !strings.Contains(out, `"deimos"`) {
		t.Fatalf("org brief without --role: exit=%d out=%s diag=%s", code, out, diag)
	}
	output, err := claimTurnHookNotifications(turnHookRequest{runtime: "claude", event: "UserPromptSubmit", home: clientHome}, turnHookStdin{}, nil, io.Discard)
	if err != nil || output == nil || output.HookSpecificOutput == nil || !strings.Contains(output.HookSpecificOutput.AdditionalContext, "Mail for the remote seat.") {
		t.Fatalf("hook without a local role: %+v %v", output, err)
	}
	// No --role and no --head: the request reaches the hub (which then needs
	// GitHub to resolve the head) instead of a local refusal or a 400 on the
	// missing head.
	code, _, diag = runCLI(t, "review", "request", "--repo", "gitmoot/implementation", "--pr", "7", "--home", clientHome)
	if code == 0 || strings.Contains(diag, "set --role") || strings.Contains(diag, "40-character") || !strings.Contains(diag, "hub:") {
		t.Fatalf("review request without --role/--head: exit=%d %s", code, diag)
	}
}

// pr show with an owner/repo#N ref reaches the hub's task handler: the ref
// travels in the query, because the hub refuses any escaped path (#2718).
func TestClientPRShowRefWithSlashAndHashReachesTheHub(t *testing.T) {
	hubHome := messageTestHome(t)
	clientHome, _ := clientTestHome(t, hubHome, "mac", "gm-omp-impl", "")
	code, _, diag := runCLI(t, "pr", "show", "gitmoot/implementation#7", "--home", clientHome)
	if code == 0 || !strings.Contains(diag, "not found") || strings.Contains(diag, "escape") || strings.Contains(diag, "HTTP 400") {
		t.Fatalf("pr show owner/repo#N through the hub: exit=%d %s", code, diag)
	}
}
