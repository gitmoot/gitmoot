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

// TestDispatchEmitsNoWarningWhenThePromptAgreesWithTheHead is the NEGATIVE integration case,
// and it is the only thing here that binds the durable event to a genuine SCAN.
//
// Review showed both substrings of the durable message are synthesizable: replacing the
// scanner call with one unconditional message built from request.Instructions plus the literal
// "Gitmoot will use dispatch head" compiled and left all four guards green. Every existing
// guard exercises a prompt that SHOULD warn, so a mutant that always warns satisfies all of
// them. Cardinality and message shape cannot distinguish "scanned and found a contradiction"
// from "emitted regardless".
//
// A prompt whose commit token EQUALS the dispatch head is resolvable, traversed, and correctly
// silent. An unconditional emitter fails here and nowhere else.
func TestDispatchEmitsNoWarningWhenThePromptAgreesWithTheHead(t *testing.T) {
	checkout, _, head, divergent := promptHeadWarningRepository(t)
	store, home := blockerE2EHome(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "printf '%s' '{}'", []string{"ask"}, "owner/repo")

	var stdout, stderr bytes.Buffer
	output, exit := dispatchAgentCommand(agentRunOptions{
		home:       home,
		repo:       "owner/repo",
		agent:      "responder",
		message:    "Use head " + head[:8] + " for this analysis.",
		background: true,
	}, "ask", "explicit", "agent_run", &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("dispatchAgentCommand exit = %d, stderr=%s", exit, stderr.String())
	}

	// PREMISE, THROUGH THE DISPATCH PATH ITSELF. The earlier version established its premises
	// by calling promptHeadWarnings directly -- which proves the HELPER is live and says
	// nothing about whether THIS dispatch ran it. Review demonstrated the gap: replacing the
	// dispatch's message with an empty string left the test green, because the premise checks
	// never went through dispatchAgentCommand at all.
	//
	// So the control is a SECOND dispatch, same command, same store and home, differing only
	// in that its prompt contains a DIVERGENT token. If that one warns and this one does not,
	// the dispatch's own scan path is armed and its silence above is conditional rather than
	// absent.
	var ctlOut, ctlErr bytes.Buffer
	control, ctlExit := dispatchAgentCommand(agentRunOptions{
		home:       home,
		repo:       "owner/repo",
		agent:      "responder",
		message:    "Use head " + divergent[:8] + " for this analysis.",
		background: true,
	}, "ask", "explicit", "agent_run", &ctlOut, &ctlErr)
	if ctlExit != 0 {
		t.Fatalf("control dispatch exit = %d, stderr=%s", ctlExit, ctlErr.String())
	}
	if got := strings.Count(ctlErr.String(), "agent ask: warning:"); got != 1 {
		t.Fatalf("control dispatch warnings = %d, want 1; the DISPATCH path does not scan at all, so the silence asserted below proves nothing; stderr=%q", got, ctlErr.String())
	}
	controlEvents, err := store.ListJobEvents(context.Background(), control.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	controlDurable := 0
	for _, event := range controlEvents {
		if event.Kind == "prompt_head_warning" {
			controlDurable++
		}
	}
	if controlDurable != 1 {
		t.Fatalf("control dispatch durable events = %d, want 1; the durable emit is not reached from this path", controlDurable)
	}

	if got := strings.Count(stderr.String(), "agent ask: warning:"); got != 0 {
		t.Fatalf("operator warnings = %d, want 0: the prompt AGREES with the dispatch head; stderr=%q", got, stderr.String())
	}
	events, err := store.ListJobEvents(context.Background(), output.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "prompt_head_warning" {
			t.Fatalf("durable prompt_head_warning emitted for a prompt that agrees with the head (message=%q); the emit is unconditional, not the product of a scan", event.Message)
		}
	}
}

// promptHeadWarningRepositoryWithDivergents builds the same fixture plus n ADDITIONAL divergent
// commits, each on its own branch off the ancestor.
//
// One divergent commit is what made traversal and identity inseparable: with a single unequal
// token, "one warning" and "the right warning" are the same observation, so a scanner that
// traversed everything but reused the first divergent label passed every guard. Several
// divergent commits split those properties apart -- the count constrains traversal, the set of
// named tokens constrains identity, and neither implies the other.
func promptHeadWarningRepositoryWithDivergents(t *testing.T, n int) (checkout string, head string, divergents []string) {
	t.Helper()
	checkout, ancestor, head, first := promptHeadWarningRepository(t)
	divergents = append(divergents, first)
	for i := 0; i < n; i++ {
		branch := fmt.Sprintf("divergent-%d", i)
		runGit(t, checkout, "checkout", "-b", branch, ancestor)
		writePromptHeadFixture(t, checkout, fmt.Sprintf("divergent %d\n", i), branch)
		divergents = append(divergents, promptHeadFixtureSHA(t, checkout))
		runGit(t, checkout, "checkout", "main")
	}
	return checkout, head, divergents
}

// TestPromptHeadWarningTraversesEveryTokenAndNamesEachOne is the traversal + identity guard,
// and it replaces a control that review showed was enumerable twice over.
//
// The previous version placed ONE divergent token at indexes 0, 4 and 12 -- all EVEN -- so a
// scanner reading every other token survived all three, and it claimed "any fixed cap misses a
// placement", which a cap above 13 disproved.
//
// SCOPE OF THIS GUARD, stated exactly, because the replacement claim was ALSO too strong and was
// caught a second time. With SIX divergent commits interleaved among non-commit filler at both
// odd and even indexes:
//
//	COUNT      constrains traversal  - a cap at any n < 6 divergents, or every-other-token
//	                                   sampling, yields fewer than six warnings.
//	NAMED SET  constrains identity   - a scanner that traverses fully but reuses one label
//	                                   still yields six warnings, and fails here.
//
// Those are the two mutants review demonstrated surviving, and they fail for DIFFERENT reasons,
// which is the point: with one divergent commit, count and identity are the same observation and
// the old fixture could not tell them apart.
//
// WHAT IT DOES NOT ESTABLISH: that arbitrary positional sampling is excluded. A mutant reading
// exactly the six indexes these divergents occupy produces exactly six warnings naming exactly
// the six right commits and PASSES. No finite fixture can rule that out, and pretending otherwise
// is how this file has now over-claimed twice. General traversal is not provable by example; what
// is provable is that these six positions and the measured mutant families are covered.
func TestPromptHeadWarningTraversesEveryTokenAndNamesEachOne(t *testing.T) {
	checkout, head, divergents := promptHeadWarningRepositoryWithDivergents(t, 5)
	if len(divergents) != 6 {
		t.Fatalf("fixture built %d divergent commits, want 6", len(divergents))
	}

	// Interleave the divergent tokens with resolvable-shaped but unresolvable filler so the
	// divergent indexes are NOT all of one parity -- the defect that let every-other-token
	// sampling survive.
	var b strings.Builder
	fmt.Fprintf(&b, "Dispatch head %s. ", head)
	for i, d := range divergents {
		fmt.Fprintf(&b, "ref %s ", d[:8])
		if i%2 == 0 {
			fmt.Fprintf(&b, "pad %064x ", i+1)
		}
	}
	prompt := b.String()

	// PREMISE: the divergent tokens really do land at both parities, or the fixture does not
	// exercise the sampler it claims to.
	tokens := promptCommitTokenRE.FindAllString(prompt, -1)
	even, odd := 0, 0
	for i, tok := range tokens {
		for _, d := range divergents {
			if tok == d[:8] {
				if i%2 == 0 {
					even++
				} else {
					odd++
				}
			}
		}
	}
	if even == 0 || odd == 0 {
		t.Fatalf("divergent tokens sit at even=%d odd=%d indexes; both parities are required or every-other-token sampling is not exercised", even, odd)
	}

	warnings := promptHeadWarnings(t, checkout, prompt, head)
	if len(warnings) != len(divergents) {
		t.Fatalf("warnings = %d, want %d (one per divergent commit): the scanner missed at least one of THESE six divergent positions.\nThis constrains the six positions below and the measured every-other-token and cap-below-six mutants. It does NOT exclude positional sampling in general -- a mutant reading exactly these six indexes passes, and a finite fixture cannot rule that out.\nprompt=%q\nwarnings=%v", len(warnings), len(divergents), prompt, warnings)
	}

	// IDENTITY: every divergent token is named exactly once. A full traversal that reuses one
	// label produces the right COUNT and the wrong SET.
	for _, d := range divergents {
		named := 0
		for _, w := range warnings {
			if strings.Contains(w, d[:8]) {
				named++
			}
		}
		if named != 1 {
			t.Fatalf("divergent %s named in %d warnings, want exactly 1: the count is right but the warnings do not identify their own commits.\nwarnings=%v", d[:8], named, warnings)
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
	// The placements vary BOTH the divergent token's position AND the total token count.
	// Position alone was not enough: every fixture happened to hold exactly five
	// commit-shaped tokens, so a scanner capped to the first five compiled and left all four
	// guards green -- the control was pinned to the fixture's WIDTH, not to traversal. Widths
	// here are 3, 7 and 13, so a cap at or below 12 misses at least one placement. A cap ABOVE
	// 13 survives, and no finite set of widths can exclude one -- that limit is stated rather
	// than papered over, because the earlier version of this comment claimed "any fixed cap
	// misses a placement" and review disproved it.
	//
	// Each placement also asserts the warning NAMES THE DIVERGENT TOKEN. Counting one warning
	// proves cardinality only: a mutant that reported tokens[0] instead of the unequal token
	// still produced exactly one warning per placement and passed everything. With one
	// divergent commit in the fixture the count LOOKS causal and is not.
	// Distinct 64-char HEX strings. An earlier version built these as
	// strings.Repeat(string(rune('a'+i)), 64), which silently stopped being
	// commit-shaped at i=6 ('g' is not hex) -- the width premise below caught it.
	filler := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		filler = append(filler, fmt.Sprintf("%064x", i+1))
	}
	for _, placement := range []struct {
		name   string
		prompt string
		tokens int
	}{
		{name: "first-of-3", tokens: 3, prompt: fmt.Sprintf("See %s. Head %s. Hash %s.", divergent[:8], head, filler[0])},
		{name: "middle-of-7", tokens: 7, prompt: fmt.Sprintf("Head %s. Hashes %s %s %s. See %s. And %s %s.", head, filler[0], filler[1], filler[2], divergent[:8], filler[3], filler[4])},
		{name: "last-of-13", tokens: 13, prompt: fmt.Sprintf("Head %s. Hashes %s %s %s %s %s %s %s %s %s %s %s. Finally %s.",
			head, filler[0], filler[1], filler[2], filler[3], filler[4], filler[5], filler[6], filler[7], filler[8], filler[9], filler[10], divergent[:8])},
	} {
		// PREMISE: the fixture really has the width it claims, so "a cap misses this one"
		// is a measured fact rather than an assumption about how the prompt renders.
		if got := len(promptCommitTokenRE.FindAllString(placement.prompt, -1)); got != placement.tokens {
			t.Fatalf("control (%s): prompt holds %d commit-shaped tokens, want %d; the width premise does not hold so a cap mutant may not be exercised", placement.name, got, placement.tokens)
		}
		live := promptHeadWarnings(t, checkout, placement.prompt, head)
		if len(live) != 1 {
			t.Fatalf("control (%s): the scanner produced %d warnings, want 1; a scanner that samples a fixed position or caps its token count is not traversing the prompt", placement.name, len(live))
		}
		if !strings.Contains(live[0], divergent[:8]) {
			t.Fatalf("control (%s): warning %q does not name the DIVERGENT token %s; one warning proves cardinality, not that the right commit was identified", placement.name, live[0], divergent[:8])
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
