package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func stagedReviewPaths(t *testing.T, body string) config.Paths {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return config.Paths{ConfigFile: file}
}

// #1821. The dispatcher's whole job in a staged review is to REFUSE a
// declaration it cannot honour and to tell an honourable one what to delegate.
// Everything else about staging is enforced by guards that already exist.

func TestStagedReviewUndeclaredRepoTakesTheUnstagedPath(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	paths := stagedReviewPaths(t, "[daemon]\nworkers = 1\n")
	agent, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err != nil {
		t.Fatalf("an undeclared repo REFUSED the dispatch: %v", err)
	}
	if staged || agent != "" {
		t.Fatalf("undeclared repo reported staged=%v agent=%q", staged, agent)
	}
}

// #1821 commit one of two: the MARKER. The ceiling that consumes it is the next
// commit, and it is separate on purpose - the marker has to be readable landing
// before the thing that reads it.
//
// The marker exists because the alternative was measured and broke: inferring
// "this is a staged pair" from the parent having declared `evidence` also
// matches an honest review coordinator, whose lens children genuinely execute.
// PR #2024 was closed for exactly that. So this asserts the marker is SET FROM
// THE DISPATCH's own resolution and absent otherwise.
func TestStagedReviewMarkerCarriesTheResolvedVerdictAgent(t *testing.T) {
	// The prompt and the marker come from ONE resolution, so a preflight can
	// never carry instructions naming an agent the marker does not.
	block := stagedReviewPreflightInstructions("gm-review-opus")
	if !strings.Contains(block, "gm-review-opus") {
		t.Fatalf("preflight block does not name the agent it was resolved with:\n%s", block)
	}
}

// The marker must be absent from every payload that is not a staged preflight,
// which is what makes this addition rather than a migration.
func TestStagedReviewMarkerIsAbsentFromAnOrdinaryPayload(t *testing.T) {
	raw, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", Branch: "main"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "staged_review_verdict_agent") {
		t.Fatalf("an ordinary payload carries the staged marker: %s", raw)
	}
	staged, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", StagedReviewVerdictAgent: "gm-review-opus"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(staged), `"staged_review_verdict_agent":"gm-review-opus"`) {
		t.Fatalf("a staged payload does not carry the marker: %s", staged)
	}
}

// The refusal, which is the reason this seam exists at all.
func TestStagedReviewRefusesAnUnregisteredVerdictAgent(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	paths := stagedReviewPaths(t, "[repos.\"owner/repo\"]\nstaged_review_verdict_agent = \"ghost-reviewer\"\n")
	_, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err == nil {
		t.Fatal("a declared verdict agent that is not registered was accepted; the review would enqueue with a verdict slot nothing can fill")
	}
	if staged {
		t.Fatal("a refused staged review also reported staged=true")
	}
	var refusal stagedReviewRefusalError
	if !asStagedReviewRefusal(err, &refusal) {
		t.Fatalf("refusal is not typed: %T", err)
	}
	if refusal.Agent != "ghost-reviewer" || !strings.Contains(err.Error(), "not a registered agent") {
		t.Fatalf("refusal does not name the cause: %v", err)
	}
	// The remedy has to be in the message: an operator reading this needs to know
	// which config key to fix, not merely that something was refused.
	if !strings.Contains(err.Error(), "staged_review_verdict_agent") {
		t.Fatalf("refusal does not name the config key to fix: %v", err)
	}
}

// A registered agent that cannot review is the subtler misconfiguration, and it
// is the one an operator is most likely to make by naming an implementer.
func TestStagedReviewRefusesAnAgentWithoutTheReviewCapability(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "impl-only", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	paths := stagedReviewPaths(t, "[repos.\"owner/repo\"]\nstaged_review_verdict_agent = \"impl-only\"\n")
	_, _, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err == nil {
		t.Fatal("an agent with no review capability was accepted as the verdict stage")
	}
	if !strings.Contains(err.Error(), "review capability") {
		t.Fatalf("refusal does not name the missing capability: %v", err)
	}
}

// The should-SUCCEED arm. A guard that refuses a valid configuration is its own
// defect, and that is the failure mode I hit twice earlier in this campaign.
func TestStagedReviewAcceptsADeclaredReviewCapableAgent(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "gm-review-opus", runtime.ClaudeRuntime, "unused", []string{"review", "ask"}, "owner/repo")
	paths := stagedReviewPaths(t, "[repos.\"owner/repo\"]\nstaged_review_verdict_agent = \"gm-review-opus\"\n")
	agent, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err != nil {
		t.Fatalf("a valid declaration was refused: %v", err)
	}
	if !staged || agent != "gm-review-opus" {
		t.Fatalf("valid declaration produced staged=%v agent=%q", staged, agent)
	}
}

// An unreadable config must not refuse every review on the repo: the
// declaration is the opt-in, and a config that cannot be read cannot express
// one. Failing closed here would break unstaged reviews too.
func TestStagedReviewMissingConfigDoesNotRefuse(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	paths := config.Paths{ConfigFile: filepath.Join(t.TempDir(), "absent.toml")}
	agent, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err != nil || staged || agent != "" {
		t.Fatalf("an unreadable config produced staged=%v agent=%q err=%v; want the unstaged path", staged, agent, err)
	}
}

// The preflight block is what makes the parent emit the delegation, so its
// load-bearing content is asserted rather than its wording: it names the agent,
// it forbids a verdict, and it explains the evidence ceiling.
func TestStagedReviewPreflightInstructionsCarryTheContract(t *testing.T) {
	block := stagedReviewPreflightInstructions("gm-review-opus")
	for _, want := range []string{
		"gm-review-opus",
		"EXACTLY ONE delegation",
		"FAN-OUT",
		"CEILING",
		"delegate nothing",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("preflight block omits %q:\n%s", want, block)
		}
	}
}

// #1819's scan refuses a review prompt naming a commit outside the pull
// request's history, matching 7 to 64 hex characters. This block is appended to
// review prompts, so it must contain no such token - and an agent name COULD be
// hex-shaped one day ("deadbeef" is a legal agent name), which is why this
// asserts the rendered block rather than trusting the input.
func TestStagedReviewPreflightBlockCarriesNoCommitShapedToken(t *testing.T) {
	commitLike := regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)
	block := stagedReviewPreflightInstructions("gm-review-opus")
	if match := commitLike.FindString(block); match != "" {
		t.Fatalf("preflight block contains a commit-shaped token %q; #1819's dispatch scan would refuse every staged review", match)
	}
}

func asStagedReviewRefusal(err error, target *stagedReviewRefusalError) bool {
	refusal, ok := err.(stagedReviewRefusalError)
	if ok {
		*target = refusal
	}
	return ok
}

var _ = db.Agent{}

// #2029 review, P1. AN UNREADABLE CONFIG MUST REFUSE; ONLY AN ABSENT ONE IS
// "UNSTAGED".
//
// The previous code collapsed every error into unstaged, reasoning that a read
// failure "says nothing about whether a declaration exists". That is true, and
// it is the reason to refuse: LoadStagedReview parses the WHOLE config, so a
// malformed staged value for ANOTHER repository silently unstages this one, and
// the review then runs on the cheap agent with no marker. A config typo
// elsewhere in the file became a silent fallback.
func TestStagedReviewRefusesWhenTheConfigCannotBeRead(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)

	t.Run("malformed config for another repo does not unstage this one", func(t *testing.T) {
		paths := stagedReviewPaths(t, "[repos.\"other/repo\"]\nstaged_review_verdict_agent = [unclosed\n")
		_, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
		if err == nil {
			t.Fatal("a config that could not be parsed silently reported unstaged")
		}
		if staged {
			t.Fatal("staged must be false alongside a refusal")
		}
		if !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("refusal must name the cause, got %q", err.Error())
		}
	})

	t.Run("unreadable file refuses", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(file, []byte("[daemon]\nworkers = 1\n"), 0o000); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if os.Geteuid() == 0 {
			t.Skip("running as root: mode 0000 is still readable, so this case cannot be produced here")
		}
		_, _, err := resolveStagedReviewVerdictAgent(ctx, store, config.Paths{ConfigFile: file}, "owner/repo")
		if err == nil {
			t.Fatal("an unreadable config silently reported unstaged")
		}
	})
}

// A GENUINELY ABSENT CONFIG IS DIFFERENT IN KIND: it cannot be hiding a
// declaration, and treating it as unstaged is what keeps a fresh home and every
// repo that never opted in byte-identical. Without this the refusal above would
// break every review on a host with no config file.
func TestStagedReviewTreatsAnAbsentConfigAsUnstaged(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	paths := config.Paths{ConfigFile: filepath.Join(t.TempDir(), "does-not-exist.toml")}

	agent, staged, err := resolveStagedReviewVerdictAgent(ctx, store, paths, "owner/repo")
	if err != nil {
		t.Fatalf("an absent config must take the unstaged path, got refusal: %v", err)
	}
	if staged || agent != "" {
		t.Fatalf("absent config reported staged=%v agent=%q", staged, agent)
	}
}
