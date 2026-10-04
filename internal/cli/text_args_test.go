package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// freeTextCommandResult runs one gitmoot command in a fresh isolated home and
// reports its output and whether it left any trace: a database (nothing is
// stored without one) or a changed config file.
func freeTextCommandResult(t *testing.T, args []string) (code int, stdout, stderr string, wrote string) {
	t.Helper()
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("GITMOOT_ORG_ROLE", "")
	home := messageTestHome(t)
	paths := config.PathsForHome(home)
	configBefore, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	code = Run(append(append([]string{}, args...), "--home", home), &out, &diag)
	if _, err := os.Stat(paths.Database); !os.IsNotExist(err) {
		wrote = "opened or created the database"
	}
	configAfter, err := os.ReadFile(paths.ConfigFile)
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		wrote = "changed the config file"
	}
	return code, out.String(), diag.String(), wrote
}

// TestFreeTextCommandsPrintHelpAndSaveNothing extends #2306 beyond messages:
// every command that saves free text (a note, description, reason, title,
// summary, prompt, message, filter or session command) prints usage on stdout
// and saves nothing when help appears after a positional or where the text
// belongs.
func TestFreeTextCommandsPrintHelpAndSaveNothing(t *testing.T) {
	cases := []struct {
		name string
		args func(help string) []string
		want string
	}{
		{"workflow-note", func(h string) []string { return []string{"workflow", "note", "wf-1", h} }, "workflow note"},
		{"workflow-describe", func(h string) []string { return []string{"workflow", "describe", "wf-1", h} }, "workflow describe"},
		{"workflow-register", func(h string) []string { return []string{"workflow", "register", "wf-1", h} }, "workflow register"},
		{"workflow-close", func(h string) []string { return []string{"workflow", "close", "wf-1", "--reason", h} }, "workflow close"},
		{"task-successor", func(h string) []string { return []string{"task", "successor", "t-1", "--reason", h} }, "task successor"},
		{"escalation-repair", func(h string) []string {
			return []string{"escalation", "repair", "job-1", "--round", "r-1", "--supersede", "--reason", h}
		}, "escalation repair"},
		{"job-open", func(h string) []string {
			return []string{"job", "open", "--agent", "a", "--repo", "o/r", "--type", "implement", "--title", h}
		}, "job open"},
		{"job-close", func(h string) []string {
			return []string{"job", "close", "job-1", "--decision", "approve", "--summary", h}
		}, "job close"},
		{"job-record", func(h string) []string {
			return []string{"job", "record", "--agent", "a", "--repo", "o/r", "--type", "implement", "--decision", "approve", "--summary", h}
		}, "job record"},
		{"agent-heartbeat-add", func(h string) []string {
			return []string{"agent", "heartbeat", "add", "a", "hb", "--repo", "o/r", "--interval", "24h", "--prompt", h}
		}, "agent heartbeat add"},
		{"agent-ask", func(h string) []string { return []string{"agent", "ask", "a", h} }, "agent ask"},
		{"agent-run", func(h string) []string { return []string{"agent", "run", "a", h} }, "agent run"},
		{"agent-review", func(h string) []string { return []string{"agent", "review", "a", h, "--repo", "o/r", "--pr", "1"} }, "agent review"},
		{"orchestrate", func(h string) []string { return []string{"orchestrate", "a", h} }, "orchestrate"},
		{"org-events-rule-add", func(h string) []string {
			return []string{"org", "events", "rule", "add", "--on", "reply", "--match", h, "--wake", "deimos"}
		}, "org events rule add"},
		{"setup", func(h string) []string {
			return []string{"setup", "--repo", "o/r", "--agent", "a", "--runtime", "shell", "--session", h}
		}, "setup"},
		{"agent-start", func(h string) []string {
			return []string{"agent", "start", "a", "--runtime", "codex", "--repo", "o/r", "--role", h}
		}, "agent start"},
		{"agent-subscribe", func(h string) []string {
			return []string{"agent", "subscribe", "a", "--runtime", "shell", "--session", h}
		}, "agent subscribe"},
		{"review-request", func(h string) []string { return []string{"review", "request", "--pr", "1", "--session", h} }, "review request"},
	}
	for _, tc := range cases {
		for _, help := range []string{"-h", "-help", "--help"} {
			t.Run(tc.name+help, func(t *testing.T) {
				code, stdout, stderr, wrote := freeTextCommandResult(t, tc.args(help))
				if wrote != "" {
					t.Fatalf("help %s; exit=%d stdout=%q stderr=%q", wrote, code, stdout, stderr)
				}
				if code != 0 || !strings.Contains(strings.ToLower(stdout), "usage") || !strings.Contains(stdout, tc.want) {
					t.Fatalf("exit=%d, want 0 with %q usage on stdout; stdout=%q stderr=%q", code, tc.want, stdout, stderr)
				}
			})
		}
	}
}

// TestFreeTextCommandsRefuseFlagLikeText pins the second #2306 rule beyond
// messages: free text that is a single flag-like word is a misplaced flag, so
// it is refused before anything is stored.
func TestFreeTextCommandsRefuseFlagLikeText(t *testing.T) {
	for _, args := range [][]string{
		{"workflow", "note", "wf-1", "-x"},
		{"workflow", "note", "wf-1", "checked the build", "--summary", "--urgent"},
		{"workflow", "describe", "wf-1", "-x"},
		{"workflow", "register", "wf-1", "-x"},
		{"workflow", "close", "wf-1", "--reason", "-x"},
		{"task", "successor", "t-1", "--reason", "-x"},
		{"escalation", "repair", "job-1", "--round", "r-1", "--supersede", "--reason", "-x"},
		{"job", "open", "--agent", "a", "--repo", "o/r", "--type", "implement", "--title", "-x"},
		{"job", "close", "job-1", "--decision", "approve", "--summary", "-x"},
		{"job", "record", "--agent", "a", "--repo", "o/r", "--type", "implement", "--decision", "approve", "--summary", "-x"},
		{"agent", "heartbeat", "add", "a", "hb", "--repo", "o/r", "--interval", "24h", "--prompt", "-x"},
		{"agent", "ask", "a", "--bogus"},
		{"agent", "run", "a", "-x"},
		{"orchestrate", "a", "-x"},
		{"org", "events", "rule", "add", "--on", "reply", "--match", "-x", "--wake", "deimos"},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			code, stdout, stderr, wrote := freeTextCommandResult(t, args)
			if wrote != "" {
				t.Fatalf("%v %s; exit=%d stdout=%q stderr=%q", args, wrote, code, stdout, stderr)
			}
			if code != 2 || !strings.Contains(stderr, "looks like a flag") {
				t.Fatalf("%v: exit=%d, want 2 with flag-like refusal; stdout=%q stderr=%q", args, code, stdout, stderr)
			}
		})
	}
}
