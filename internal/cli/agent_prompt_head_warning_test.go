package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestDispatchPromptHeadWarningReachesOperator(t *testing.T) {
	checkout, _, _, divergent := promptHeadWarningRepository(t)
	store, home := blockerE2EHome(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "printf '%s' '{}'", []string{"ask"}, "owner/repo")

	var stdout, stderr bytes.Buffer
	_, exit := dispatchAgentCommand(agentRunOptions{
		home:       home,
		repo:       "owner/repo",
		agent:      "responder",
		message:    "Use head " + divergent[:8] + " for this analysis.",
		background: true,
	}, "ask", "explicit", "agent_run", &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("dispatchAgentCommand exit = %d, stderr=%s", exit, stderr.String())
	}
	if got := strings.Count(stderr.String(), "agent ask: warning:"); got != 1 {
		t.Fatalf("operator warnings = %d, want 1; stderr=%q", got, stderr.String())
	}
}

func TestPromptHeadWarningUsesInequalityNotReachability(t *testing.T) {
	checkout, ancestor, head, _ := promptHeadWarningRepository(t)
	warnings := promptHeadWarnings(t, checkout, "The reviewed head was "+ancestor[:8]+".", head)
	if len(warnings) != 1 {
		t.Fatalf("ancestor prompt warnings = %d, want 1: %v", len(warnings), warnings)
	}
}

func TestPromptHeadWarningIgnoresMutationRestoreHashes(t *testing.T) {
	checkout, _, head, _ := promptHeadWarningRepository(t)
	prompt := fmt.Sprintf(`Head %s. Restore hashes: %s %s %s.`, head,
		strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64))
	if warnings := promptHeadWarnings(t, checkout, prompt, head); len(warnings) != 0 {
		t.Fatalf("mutation-hygiene prompt warnings = %d, want 0: %v", len(warnings), warnings)
	}
}

func TestPromptHeadWarningNamesBothCommitsAndWinner(t *testing.T) {
	checkout, _, head, divergent := promptHeadWarningRepository(t)
	token := divergent[:8]
	warnings := promptHeadWarnings(t, checkout, "Head "+token, head)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1: %v", len(warnings), warnings)
	}
	warning := warnings[0]
	for _, required := range []string{token, head, "Gitmoot will use dispatch head " + head} {
		if !strings.Contains(warning, required) {
			t.Fatalf("warning %q does not contain %q", warning, required)
		}
	}
}

func promptHeadWarnings(t *testing.T, checkout string, prompt string, head string) []string {
	t.Helper()
	return promptHeadContradictionWarnings(context.Background(), gitutil.Client{Dir: checkout}, prompt, head)
}

func promptHeadWarningRepository(t *testing.T) (checkout string, ancestor string, head string, divergent string) {
	t.Helper()
	checkout = t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	writePromptHeadFixture(t, checkout, "base\n", "base")
	ancestor = promptHeadFixtureSHA(t, checkout)
	writePromptHeadFixture(t, checkout, "dispatch head\n", "dispatch head")
	head = promptHeadFixtureSHA(t, checkout)
	runGit(t, checkout, "checkout", "-b", "divergent", ancestor)
	writePromptHeadFixture(t, checkout, "divergent\n", "divergent")
	divergent = promptHeadFixtureSHA(t, checkout)
	runGit(t, checkout, "checkout", "main")
	return checkout, ancestor, head, divergent
}

func writePromptHeadFixture(t *testing.T, checkout string, contents string, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(checkout, "state.txt"), []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, checkout, "add", "state.txt")
	runGit(t, checkout, "commit", "-m", message)
}

func promptHeadFixtureSHA(t *testing.T, checkout string) string {
	t.Helper()
	sha, err := (gitutil.Client{Dir: checkout}).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return sha
}
