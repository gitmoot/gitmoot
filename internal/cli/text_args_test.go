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
		{"note-add", func(h string) []string { return []string{"note", "add", "wf-1", h} }, "note add"},
		{"campaign-describe", func(h string) []string { return []string{"campaign", "describe", "wf-1", h} }, "campaign describe"},
		{"campaign-register", func(h string) []string { return []string{"campaign", "register", "wf-1", h} }, "campaign register"},
		{"campaign-close", func(h string) []string { return []string{"campaign", "close", "wf-1", "--reason", h} }, "campaign close"},
		{"pr-successor", func(h string) []string { return []string{"pr", "successor", "t-1", "--reason", h} }, "pr successor"},
		{"job-open", func(h string) []string {
			return []string{"run", "open", "--agent", "a", "--repo", "o/r", "--type", "implement", "--title", h}
		}, "run open"},
		{"job-close", func(h string) []string {
			return []string{"run", "close", "job-1", "--decision", "approve", "--summary", h}
		}, "run close"},
		{"job-record", func(h string) []string {
			return []string{"run", "record", "--agent", "a", "--repo", "o/r", "--type", "implement", "--decision", "approve", "--summary", h}
		}, "run record"},
		{"agent-ask", func(h string) []string { return []string{"ask", "a", h} }, "gitmoot ask"},
		{"agent-review", func(h string) []string { return []string{"reviewer", "review", "a", h, "--repo", "o/r", "--pr", "1"} }, "agent review"},
		{"org-events-rule-add", func(h string) []string {
			return []string{"org", "events", "rule", "add", "--on", "reply", "--match", h, "--wake", "deimos"}
		}, "org events rule add"},
		{"setup", func(h string) []string {
			return []string{"setup", "--repo", "o/r", "--reviewer", "a", "--runtime", "shell", "--session", h}
		}, "setup"},
		{"agent-start", func(h string) []string {
			return []string{"reviewer", "start", "a", "--runtime", "codex", "--repo", "o/r", "--role", h}
		}, "reviewer start"},
		{"agent-subscribe", func(h string) []string {
			return []string{"reviewer", "subscribe", "a", "--runtime", "shell", "--session", h}
		}, "reviewer subscribe"},
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
		{"note", "add", "wf-1", "-x"},
		{"note", "add", "wf-1", "checked the build", "--summary", "--urgent"},
		{"campaign", "describe", "wf-1", "-x"},
		{"campaign", "register", "wf-1", "-x"},
		{"campaign", "close", "wf-1", "--reason", "-x"},
		{"pr", "successor", "t-1", "--reason", "-x"},
		{"run", "open", "--agent", "a", "--repo", "o/r", "--type", "implement", "--title", "-x"},
		{"run", "close", "job-1", "--decision", "approve", "--summary", "-x"},
		{"run", "record", "--agent", "a", "--repo", "o/r", "--type", "implement", "--decision", "approve", "--summary", "-x"},
		{"ask", "a", "--bogus"},
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
