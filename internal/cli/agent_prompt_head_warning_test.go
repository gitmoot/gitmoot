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
	output, exit := dispatchAgentCommand(agentRunOptions{
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

	// AND THE DURABLE SURFACE, BOUND TO THE JOB THE OPERATOR WAS HANDED. stderr reaches
	// only someone watching the dispatch; a BACKGROUND job's warning is read later, from
	// job events -- and the operator finds those events using the JobID this call RETURNED,
	// which is what the printed `gitmoot job watch <id>` command carries.
	//
	// An earlier version queried ListJobs()[0] and discarded the returned output. That made
	// the assertion true of "some job in the store" rather than of THIS job: a mutant
	// returning job.ID + "-wrong" persisted the event on the real row, passed, and would
	// have sent the operator to a job id that does not exist. With one job in the store the
	// two readings coincide, which is exactly what made the gap invisible.
	if strings.TrimSpace(output.JobID) == "" {
		t.Fatal("dispatch returned no JobID, so the operator has no handle to read the durable warning with")
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("no job was created, so this test cannot observe the durable warning")
	}
	// The returned handle must name a job that EXISTS. Reading events for an unknown id
	// returns an empty slice, not an error, so without this the count assertion below would
	// report "0 events" and read as a missing emit rather than a bad handle.
	if _, err := store.GetJob(context.Background(), output.JobID); err != nil {
		t.Fatalf("GetJob(%q) returned error: %v; the returned JobID does not name a real job", output.JobID, err)
	}
	events, err := store.ListJobEvents(context.Background(), output.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	durable := 0
	var durableMessage string
	for _, event := range events {
		if event.Kind == "prompt_head_warning" {
			durable++
			durableMessage = event.Message
		}
	}
	if durable != 1 {
		t.Fatalf("durable prompt_head_warning events on job %s = %d, want 1; the background operator surface is unguarded", output.JobID, durable)
	}
	// And the stored message must carry the contradiction, not merely the kind. A kind-only
	// assertion is satisfied by an empty message, which tells a later reader nothing.
	for _, required := range []string{divergent[:8], "Gitmoot will use dispatch head"} {
		if !strings.Contains(durableMessage, required) {
			t.Fatalf("durable warning %q does not contain %q", durableMessage, required)
		}
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
	checkout, _, head, divergent := promptHeadWarningRepository(t)
	hashes := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	prompt := fmt.Sprintf(`Head %s. Restore hashes: %s %s %s.`, head, hashes[0], hashes[1], hashes[2])

	// ASSERT THE PREMISE. "Zero warnings" is trivially true when there are no restore
	// hashes to ignore: replacing all three with "No restore hashes." left this test
	// green, so it could not witness the behaviour it names.
	for _, hash := range hashes {
		// Assert the PROPERTY, not the presence. Checking `strings.Contains(prompt, hash)`
		// follows the mutant: replacing the hashes with "No restore hashes." leaves them
		// present in the prompt, so that check passes and the premise is not established.
		// What matters is that each is a 64-char hex string -- a mutation-hygiene restore
		// hash -- because that is the shape the scanner must decline to warn about.
		if len(hash) != 64 {
			t.Fatalf("premise not established: %q is %d chars, not a 64-char restore hash", hash, len(hash))
		}
		for _, r := range hash {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				t.Fatalf("premise not established: %q is not hex, so it is not a restore hash", hash)
			}
		}
		if !strings.Contains(prompt, hash) {
			t.Fatalf("premise not established: prompt does not contain %s...", hash[:8])
		}
	}

	// AND PROVE THE SCANNER IS LIVE ON THIS PROMPT, AT EVERY TOKEN POSITION.
	//
	// A single control appending the divergent token to the END of the prompt was not
	// enough, and the way it failed is worth stating: a scanner mutated to examine only
	// tokens[len(tokens)-1:] compiled and left ALL FOUR of this file's tests green. In the
	// control the divergent token IS the last token, so it warned; in the baseline the last
	// token is a restore hash, so it did not. Both assertions were satisfied by a scanner
	// that reads exactly one token, which is not a scanner.
	//
	// So the control is now positional: the SAME divergent token is placed FIRST, in the
	// MIDDLE and LAST, and each placement must produce exactly one warning. Any mutant that
	// samples a fixed position instead of traversing fails at least two of the three.
	for _, placement := range []struct {
		name   string
		prompt string
	}{
		{name: "first", prompt: "See " + divergent[:8] + ". " + prompt},
		{name: "middle", prompt: fmt.Sprintf("Head %s. Restore hashes: %s %s. See %s. And %s.", head, hashes[0], hashes[1], divergent[:8], hashes[2])},
		{name: "last", prompt: prompt + " See also " + divergent[:8] + "."},
	} {
		live := promptHeadWarnings(t, checkout, placement.prompt, head)
		if len(live) != 1 {
			t.Fatalf("control (%s placement): the scanner produced %d warnings, want 1; a scanner that samples one position is not traversing the prompt", placement.name, len(live))
		}
	}

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
