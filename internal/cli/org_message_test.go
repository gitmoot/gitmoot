package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func orgMessageTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	configBody := `[org.roles."owner"]
scope=["*"]
[org.roles."gitmoot"]
parent="owner"
scope=["*"]
[org.roles."jarvis"]
parent="owner"
scope=["*"]
[org.roles."deimos"]
parent="owner"
scope=["*"]
[org.roles."gm-omp-nag"]
parent="gitmoot"
scope=["gitmoot/nag"]
[org.roles."gm-omp-impl"]
parent="gitmoot"
scope=["gitmoot/implementation"]
[org.roles."gm-omp-verdict"]
parent="gitmoot"
scope=["gitmoot/review"]
`
	if err := os.WriteFile(paths.ConfigFile, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestOrgMessageSendAllowsDifferentlyScopedSameParentSiblings(t *testing.T) {
	home := orgMessageTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "gm-omp-nag")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"message", "send", "--home", home,
		"--to", "gm-omp-impl",
		"--workflow", "gitmoot/1692-test",
		"Our open PRs touch internal/cli/org.go",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sibling send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notes, err := store.ListWorkflowNotes(context.Background(), "gitmoot/1692-test", 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes=%+v err=%v, want one durable message", notes, err)
	}
	note := notes[0]
	from, to, workflowID, message, ok := workflow.ParseOrgMessageNote(note.Body)
	if !ok || from != "gm-omp-nag" || to != "gm-omp-impl" || workflowID != "gitmoot/1692-test" || message != "Our open PRs touch internal/cli/org.go" {
		t.Fatalf("parsed message=(from=%q to=%q workflow=%q message=%q ok=%v)", from, to, workflowID, message, ok)
	}
	if note.Author != from {
		t.Fatalf("durable author=%q, want sender %q", note.Author, from)
	}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending wakes=%+v err=%v, want one direct wake", pending, err)
	}
	if pending[0].SourceKind != db.WakeOutboxSourceWorkflowNote || pending[0].TargetRole != "gm-omp-impl" || pending[0].CoalesceKey != db.WakeOutboxReplyCoalescePrefix+"gm-omp-impl" {
		t.Fatalf("direct wake=%+v", pending[0])
	}
	unacknowledged, err := store.ListUnacknowledgedOrgDirectives(context.Background(), "gm-omp-impl")
	if err != nil || len(unacknowledged) != 0 {
		t.Fatalf("message created directive obligations=%+v err=%v", unacknowledged, err)
	}
}

func TestOrgMessageSendAllowsOwnerChildrenAsOrdinarySiblings(t *testing.T) {
	home := orgMessageTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "jarvis")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"message", "send", "--home", home,
		"--to", "deimos",
		"--workflow", "gitmoot/1692-owner-children",
		"Owner children use the ordinary sibling predicate",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("owner-child sibling send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgMessageSendRefusesOwnerAsEndpoint(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		want string
	}{
		{name: "owner sender", from: "owner", to: "jarvis", want: `roles "owner" and "jarvis" do not share a parent`},
		{name: "owner recipient", from: "jarvis", to: "owner", want: `roles "jarvis" and "owner" do not share a parent`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := orgMessageTestHome(t)
			t.Setenv("GITMOOT_ORG_ROLE", test.from)
			var stdout, stderr bytes.Buffer
			code := runOrg([]string{
				"message", "send", "--home", home,
				"--to", test.to,
				"--workflow", "gitmoot/1692-owner-endpoint",
				"Owner has no parent and gets no direct bypass",
			}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("owner endpoint send code=%d out=%q err=%q, want %q", code, stdout.String(), stderr.String(), test.want)
			}
		})
	}
}

func TestOrgMessageSendRefusesDifferentParentDespiteWildcardScope(t *testing.T) {
	home := orgMessageTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "gm-omp-nag")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"message", "send", "--home", home,
		"--to", "jarvis",
		"--workflow", "gitmoot/1692-cross-parent",
		"Scopes must not grant this channel",
	}, &stdout, &stderr)
	want := `roles "gm-omp-nag" and "jarvis" do not share a parent`
	if code != 2 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("cross-parent send code=%d out=%q err=%q, want %q", code, stdout.String(), stderr.String(), want)
	}
}

func TestOrgMessageSendRefusesSelf(t *testing.T) {
	home := orgMessageTestHome(t)
	t.Setenv("GITMOOT_ORG_ROLE", "gm-omp-nag")
	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"message", "send", "--home", home,
		"--to", "gm-omp-nag",
		"--workflow", "gitmoot/1692-self",
		"Self is not a second role",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `must differ from acting role "gm-omp-nag"`) {
		t.Fatalf("self send code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}
