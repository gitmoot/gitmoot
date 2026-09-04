package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1856 caller tests. Every fixture is a COPY under t.TempDir() pointed at
// through agent.RuntimeConfigDir: the live profile is never read and never
// written by these tests, and no kimi binary runs.
const (
	kimiDetectorLiveCredential    = `{"access_token":"tok","refresh_token":"ref","expires_at":1788000000,"scope":"coding","token_type":"Bearer"}`
	kimiDetectorBlankedCredential = `{"access_token":"","refresh_token":"","expires_at":0,"scope":"coding","token_type":"Bearer"}`
)

func writeKimiDetectorCredential(t *testing.T, profileDir, body string) string {
	t.Helper()
	path := filepath.Join(profileDir, runtime.KimiCredentialRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	return path
}

func kimiDetectorEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var out []db.JobEvent
	for _, event := range events {
		if event.Kind == kimiCredentialDegradedEvent {
			out = append(out, event)
		}
	}
	return out
}

// TestNonSeatKimiDeliveryRecordsCredentialDegradation is the instrument's
// reason to exist: the credential carried a token before the run and none
// after, and the run SUCCEEDED. #1856's own measured window contains a kimi job
// that succeeded at 13:59:50Z shortly before the blanking, so an instrument
// that only reported on failures would miss the shape it was built for.
func TestNonSeatKimiDeliveryRecordsCredentialDegradation(t *testing.T) {
	store := daemonWorkerStore(t)
	profile := t.TempDir()
	path := writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	agent := runtime.Agent{Name: "kimi-impl", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := observeNonSeatKimiCredential(agent)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-run observation = %+v observed = %v, want a token", before, observed)
	}
	// The vendor CLI blanking its own credential in place - the measured #1856
	// shape, reproduced without running kimi.
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)

	recordKimiCredentialDegradation(context.Background(), store, io.Discard, "job-kimi-1", agent, before, observed)

	events := kimiDetectorEvents(t, store, "job-kimi-1")
	if len(events) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", kimiCredentialDegradedEvent, len(events))
	}
	message := events[0].Message
	for _, want := range []string{path, "token/", "blank_token/", "INFERRED"} {
		if !strings.Contains(message, want) {
			t.Fatalf("event message %q is missing %q", message, want)
		}
	}
	// Report-only: the observation names the file, never its contents.
	if strings.Contains(message, "tok") && !strings.Contains(message, "kimi-code.json") {
		t.Fatalf("event message may not carry credential contents: %q", message)
	}
}

// TestKimiCredentialDegradationIsRecordedAtMostOncePerJob pins the bound the
// #1856 ruling states explicitly. The two CLI dispatch boundaries are mutually
// exclusive arms of one if/else, so they cannot double-fire today - but the
// bound is enforced in the recorder rather than left to that control flow, so a
// retried job re-entering delivery cannot append a second copy of the same
// observation and a future caller cannot reintroduce the duplicate.
func TestKimiCredentialDegradationIsRecordedAtMostOncePerJob(t *testing.T) {
	store := daemonWorkerStore(t)
	profile := t.TempDir()
	writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	agent := runtime.Agent{Name: "kimi-impl", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := observeNonSeatKimiCredential(agent)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-run observation = %+v observed = %v, want a token", before, observed)
	}
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
	for range 3 {
		recordKimiCredentialDegradation(context.Background(), store, io.Discard, "job-once", agent, before, observed)
	}
	if events := kimiDetectorEvents(t, store, "job-once"); len(events) != 1 {
		t.Fatalf("%s events = %d after three recordings, want exactly 1", kimiCredentialDegradedEvent, len(events))
	}
}

// TestDelegatedKimiDeliveryRecordsCredentialDegradation covers the SECOND
// bracketed delivery route (daemon_worker.go's delegated-job path). A
// delegation child whose action is neither ask nor review is never given a
// read-only seat (engine_delegation.go sets ReadOnlySeat only for those two),
// so its kimi child reads the LIVE profile and belongs to the observed
// population. The route runs through the SAME recorder, so this test pins the
// route's coverage rather than re-testing the predicate.
func TestDelegatedKimiDeliveryRecordsCredentialDegradation(t *testing.T) {
	store := daemonWorkerStore(t)
	profile := t.TempDir()
	writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	// A delegation child's own agent, as the delegated path passes it
	// (started.Agent): an implement action, so no seat.
	delegated := runtime.Agent{Name: "kimi-delegate", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := observeNonSeatKimiCredential(delegated)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-run observation = %+v observed = %v, want a token", before, observed)
	}
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
	recordKimiCredentialDegradation(context.Background(), store, io.Discard, "delegated-kimi-1", delegated, before, observed)

	if events := kimiDetectorEvents(t, store, "delegated-kimi-1"); len(events) != 1 {
		t.Fatalf("%s events = %d, want exactly 1 for the delegated route", kimiCredentialDegradedEvent, len(events))
	}
}

// TestReadOnlySeatKimiDeliveryIsNotObserved pins the POPULATION as a property
// of the code. A seat reads a staged clone inside its own writable cache root,
// so a change there says nothing about the operator's profile; observing it
// would emit events indistinguishable from real ones.
func TestReadOnlySeatKimiDeliveryIsNotObserved(t *testing.T) {
	store := daemonWorkerStore(t)
	profile := t.TempDir()
	writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)

	for _, tc := range []struct {
		name  string
		agent runtime.Agent
	}{
		{"read-only seat", runtime.Agent{Name: "kimi-seat", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile, ReadOnlySeat: true}},
		{"another runtime", runtime.Agent{Name: "claude", Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: profile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, observed := observeNonSeatKimiCredential(tc.agent)
			if observed {
				t.Fatalf("observation = %+v, want none for %s", before, tc.name)
			}
			writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
			jobID := "job-" + tc.name
			recordKimiCredentialDegradation(context.Background(), store, io.Discard, jobID, tc.agent, before, observed)
			if events := kimiDetectorEvents(t, store, jobID); len(events) != 0 {
				t.Fatalf("%s events = %d, want 0 for %s", kimiCredentialDegradedEvent, len(events), tc.name)
			}
			writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
		})
	}
}

// TestNonSeatKimiDeliveryStaysSilentWithoutADegradation keeps the instrument
// quiet on the cases that are not the defect: an unchanged credential, and a
// host that was already logged out before the run.
func TestNonSeatKimiDeliveryStaysSilentWithoutADegradation(t *testing.T) {
	store := daemonWorkerStore(t)

	for _, tc := range []struct {
		name   string
		before string
		after  string
	}{
		{"unchanged token", kimiDetectorLiveCredential, kimiDetectorLiveCredential},
		{"already blank", kimiDetectorBlankedCredential, kimiDetectorBlankedCredential},
		{"login during the run", kimiDetectorBlankedCredential, kimiDetectorLiveCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			writeKimiDetectorCredential(t, profile, tc.before)
			agent := runtime.Agent{Name: "kimi-impl", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}
			observation, observed := observeNonSeatKimiCredential(agent)
			if !observed {
				t.Fatal("expected an observation for a non-seat kimi agent")
			}
			writeKimiDetectorCredential(t, profile, tc.after)
			jobID := "job-silent-" + tc.name
			recordKimiCredentialDegradation(context.Background(), store, io.Discard, jobID, agent, observation, observed)
			if events := kimiDetectorEvents(t, store, jobID); len(events) != 0 {
				t.Fatalf("%s events = %d, want 0", kimiCredentialDegradedEvent, len(events))
			}
		})
	}
}

// TestAgentDoctorObservesTheLiveKimiCredential covers caller 1: `gitmoot agent
// doctor` runs the runtime CLI through the plain runner with no seat sandbox, so
// its child reads the LIVE profile. Doctor is a command rather than a job, so
// the observation is reported on stderr and there is no job row to attach.
func TestAgentDoctorObservesTheLiveKimiCredential(t *testing.T) {
	profile := t.TempDir()
	path := writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	agent := runtime.Agent{Name: "gm-review-kimi", Runtime: runtime.KimiRuntime, RuntimeConfigDir: profile}

	before, observed := observeKimiDoctorCredential(agent)
	if !observed || !before.HasToken() {
		t.Fatalf("pre-check observation = %+v observed = %v, want a token", before, observed)
	}
	writeKimiDetectorCredential(t, profile, kimiDetectorBlankedCredential)
	degraded := kimiCredentialDegradationSinceDoctor(agent, before, observed)
	if degraded == "" {
		t.Fatal("a blanked credential across the doctor check must be reported")
	}
	for _, want := range []string{path, "blank_token/", "INFERRED"} {
		if !strings.Contains(degraded, want) {
			t.Fatalf("report %q is missing %q", degraded, want)
		}
	}
	// A non-kimi agent is not observed at all, so doctor's other runtimes keep
	// their byte-identical output.
	claude := runtime.Agent{Name: "claude", Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: profile}
	if _, ok := observeKimiDoctorCredential(claude); ok {
		t.Fatal("observeKimiDoctorCredential must not observe a non-kimi runtime")
	}
	if report := kimiCredentialDegradationSinceDoctor(claude, before, false); report != "" {
		t.Fatalf("unobserved agent produced a report: %q", report)
	}
}

// dispatchArmCredentialProfile redirects HOME to a throwaway profile and writes
// a live-looking credential into it.
//
// The HOME redirect is load-bearing rather than cosmetic: db.Agent carries no
// RuntimeConfigDir and runtimeAgent sets none, so effectiveAgent.RuntimeConfigDir
// is EMPTY on the dispatch path and observeNonSeatKimiCredential falls back to
// $HOME/.kimi-code. Without t.Setenv("HOME", ...) these tests would read the
// operator's real profile, which is the one thing #1856's work must never do.
func dispatchArmCredentialProfile(t *testing.T) string {
	t.Helper()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	profile := filepath.Join(fakeHome, ".kimi-code")
	writeKimiDetectorCredential(t, profile, kimiDetectorLiveCredential)
	return filepath.Join(profile, runtime.KimiCredentialRelPath)
}

// dispatchArmKimiAdapter returns a fake adapter that blanks the credential IN
// PLACE while it "delivers" - the measured #1856 shape, reproduced without a
// kimi binary through the package-level adapter seam five other test files
// already use (localAgentDispatchRuntimeAdapterFor).
func dispatchArmKimiAdapter(t *testing.T, credentialPath string) *cliWorkerFakeAdapter {
	t.Helper()
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	adapter.onDeliver = func() {
		if err := os.WriteFile(credentialPath, []byte(kimiDetectorBlankedCredential), 0o600); err != nil {
			t.Errorf("blanking the fixture credential returned error: %v", err)
		}
	}
	previousAdapter := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(string, runtime.Agent, string) (runtime.Adapter, error) { return adapter, nil }
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapter })
	previousPreflight := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{Runtime: runtime.KimiRuntime, Version: "fake", State: runtime.RuntimeContractUnknown, Instrument: "test"}
	}
	t.Cleanup(func() { localRuntimeContractPreflight = previousPreflight })
	return adapter
}

// TestCLIDispatchArmsObserveCredentialBlanking is the behavioural arm my first
// PR body wrongly called impossible: it drives dispatchLocalAgentJob end to end
// on BOTH local child-delivery boundaries and asserts the event.
//
// The two arms are separate cases because they are separate code paths, not one
// path with a flag: an ask delivers through mailbox.Run (agent_dispatch.go's
// then-branch) and never reaches engine.RunJob (the else branch). Deleting
// either recorder call fails exactly one of these cases, which is what makes the
// PR's coverage claim self-checking instead of an argument from symmetry.
func TestCLIDispatchArmsObserveCredentialBlanking(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     string
		capability string
	}{
		{"ask arm delivers through mailbox.Run", "ask", "ask"},
		{"else arm delivers through engine.RunJob", "implement", "implement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			seedDaemonWorkerAgent(t, store, "kimi-responder", runtime.KimiRuntime, "fresh", []string{tc.capability}, "owner/repo")
			credentialPath := dispatchArmCredentialProfile(t)
			dispatchArmKimiAdapter(t, credentialPath)

			out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
				RepoFlag:     "owner/repo",
				Agent:        "kimi-responder",
				Action:       tc.action,
				Instructions: "work",
				Home:         home,
			})
			if err != nil {
				t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
			}
			events := kimiDetectorEvents(t, store, out.JobID)
			if len(events) != 1 {
				t.Fatalf("%s events = %d, want exactly 1 on the %s arm", kimiCredentialDegradedEvent, len(events), tc.action)
			}
			message := events[0].Message
			for _, want := range []string{credentialPath, "token/", "blank_token/", "INFERRED"} {
				if !strings.Contains(message, want) {
					t.Fatalf("event message %q is missing %q", message, want)
				}
			}
			if strings.Contains(message, "tok") && !strings.Contains(message, "kimi-code.json") {
				t.Fatalf("event message may not carry credential contents: %q", message)
			}
		})
	}
}

// TestForegroundReviewKimiDispatchIsSeatExcluded is an EXCLUSION test and is
// labelled as one deliberately: it asserts ZERO events, so it would still pass
// if a bracket were deleted, and it must never be read as coverage of one.
//
// What it does prove, at the real CLI dispatch route rather than in the unit
// table, is the truth table for this file:
//
//	Action "ask"       -> arm 1 (mailbox.Run), non-seat  -> observed
//	Action "implement" -> arm 2 (engine.RunJob), non-seat -> observed
//	Action "review"    -> arm 2, but a read-only SEAT     -> NOT observed
//
// A foreground review allocates a read-only worktree, so effectiveAgent
// carries ReadOnlySeat=true and the observer declines. The next person to
// extend this file will reach for "review" as the obvious else-arm case (a
// coordinator's probe did exactly that and measured zero events); this test
// records why that is the exclusion rather than the bracket.
func TestForegroundReviewKimiDispatchIsSeatExcluded(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "kimi-reviewer", runtime.KimiRuntime, "fresh", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	credentialPath := dispatchArmCredentialProfile(t)
	dispatchArmKimiAdapter(t, credentialPath)
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "kimi-reviewer", LeadAgent: "lead",
		Action: "review", Instructions: "review the head", PullRequest: 7,
		Branch: "main", HeadSHA: readonlyWorktreeHead(t, checkout), Home: home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
	}
	events, err := store.ListJobEvents(ctx, out.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	seat := false
	for _, event := range events {
		if event.Kind == "readonly_worktree_allocated" {
			seat = true
		}
		if event.Kind == kimiCredentialDegradedEvent {
			t.Fatalf("a read-only seat dispatch must not be observed: %s", event.Message)
		}
	}
	// Without this the test could pass by never having taken the seat path at
	// all, which would make its zero-event assertion vacuous.
	if !seat {
		t.Fatal("expected a readonly_worktree_allocated event: the review dispatch did not take the seat path, so the exclusion was not exercised")
	}
}
