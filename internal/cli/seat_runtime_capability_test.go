package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/toolchain"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The measured #1817 condition on this host, reproduced as a fixture.
//
// stageSeatRuntimes resolves each runtime on the host PATH and stages a
// daemon-owned copy; ANY staging failure publishes that runtime name as the
// engine's exit-126 command instead, and the seat's PATH then resolves the
// stub. On the live deployment the staging failure was ELOOP resolving an
// already-published launcher, and the consequence was measured over 10 jobs on
// 2026-09-07: all 10 were given a read-only worktree, 4 posted a public
// attributed PR comment, every one died with `delivery failed: gitmoot: runtime
// unavailable: no daemon-staged artifact exists: exit status 126`, for 4,048
// seconds of queued-to-failed wall time, and none was refused first.
//
// The fixture reproduces the CLASS, not that one errno: an entrypoint whose
// shebang names an unresolvable interpreter is one of the failures
// stageSeatRuntimes documents as publishing unavailability, and unlike ELOOP it
// is deterministic on a host with or without the real CLI installed.
const unstageableRuntimeFixture = "#!/gitmoot-absent-interpreter-1817/sh\nexit 0\n"

// stageableRuntimeFixture is the POSITIVE CONTROL. Its interpreter is a real
// system one, so staging succeeds and the predicate must stay silent. Without
// this arm a predicate that reported every seat unavailable would pass.
const stageableRuntimeFixture = "#!/bin/sh\nexit 0\n"

func writeRuntimeFixture(t *testing.T, dir string, name string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// prependRuntimeFixturePath puts the fixtures FIRST, so LookPath answers with
// them whether or not this host has the real CLI installed. The rest of PATH is
// kept because seat setup and the git fixtures need ordinary system commands.
func prependRuntimeFixturePath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", strings.Join([]string{dir, os.Getenv("PATH")}, string(os.PathListSeparator)))
}

func seatFixtureCheckout(t *testing.T) string {
	t.Helper()
	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")
	return checkout
}

// TestSeatSetupReportsWhenTheSeatsOwnRuntimeIsPublishedUnavailable drives the
// production seat-setup entry, readOnlyRuntimeSandboxGrants, which is the
// function that decides what command the seat will resolve.
//
// Before the fix it reported nothing: the caller went on to compose an adapter,
// mark the agent running, and hand the seat a PATH whose runtime prints one
// line to stderr and exits 126.
func TestSeatSetupReportsWhenTheSeatsOwnRuntimeIsPublishedUnavailable(t *testing.T) {
	checkout := seatFixtureCheckout(t)
	seat := func(runtimeName string) runtime.Agent {
		return runtime.Agent{Name: "gm-review-opus", Runtime: runtimeName, ReadOnlySeat: true}
	}

	t.Run("own runtime unavailable is reported with its cause", func(t *testing.T) {
		hostBin := t.TempDir()
		writeRuntimeFixture(t, hostBin, "claude", unstageableRuntimeFixture)
		prependRuntimeFixturePath(t, hostBin)

		grants, err := readOnlyRuntimeSandboxGrants(t.TempDir(), seat(runtime.ClaudeRuntime), checkout, "gitmoot/gitmoot", true)
		if err != nil {
			t.Fatalf("seat setup returned an error rather than reporting the capability: %v", err)
		}
		if grants.runtimeUnavailable == "" {
			t.Fatalf("seat setup reported nothing for a runtime published as the exit-126 shim.\nThe seat's PATH resolves that stub, so every command this job runs would exit 126 without running.")
		}
		// The CAUSE is the only part that tells an operator what to repair. The
		// exit-126 stderr line the seat produces today carries none of it.
		if !strings.Contains(grants.runtimeUnavailable, "claude") || !strings.Contains(grants.runtimeUnavailable, "absent-interpreter-1817") {
			t.Errorf("cause %q does not name the runtime and the staging failure", grants.runtimeUnavailable)
		}
	})

	t.Run("own runtime stages so nothing is reported", func(t *testing.T) {
		hostBin := t.TempDir()
		writeRuntimeFixture(t, hostBin, "claude", stageableRuntimeFixture)
		prependRuntimeFixturePath(t, hostBin)

		grants, err := readOnlyRuntimeSandboxGrants(t.TempDir(), seat(runtime.ClaudeRuntime), checkout, "gitmoot/gitmoot", true)
		if err != nil {
			t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
		}
		if grants.runtimeUnavailable != "" {
			t.Fatalf("a runtime that staged normally was reported unavailable: %q", grants.runtimeUnavailable)
		}
	})

	t.Run("a sibling runtime being unavailable reports nothing for this seat", func(t *testing.T) {
		// SCOPE CONTROL. Every seat stages all three runtime classes, so a host
		// missing kimi would report every claude seat unavailable if the
		// predicate keyed on the staging pass rather than on the seat's own
		// runtime.
		hostBin := t.TempDir()
		writeRuntimeFixture(t, hostBin, "claude", stageableRuntimeFixture)
		writeRuntimeFixture(t, hostBin, "kimi", unstageableRuntimeFixture)
		prependRuntimeFixturePath(t, hostBin)

		grants, err := readOnlyRuntimeSandboxGrants(t.TempDir(), seat(runtime.ClaudeRuntime), checkout, "gitmoot/gitmoot", true)
		if err != nil {
			t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
		}
		if grants.runtimeUnavailable != "" {
			t.Fatalf("an unavailable SIBLING runtime was reported against a claude seat: %q", grants.runtimeUnavailable)
		}
	})
}

// seedUnavailableRuntimeSeatJob builds the worker-route fixture shared by the
// two arms below: a claude review seat on a host whose claude cannot be staged.
func seedUnavailableRuntimeSeatJob(t *testing.T, jobID string) (*db.Store, string, string) {
	t.Helper()
	hostBin := t.TempDir()
	writeRuntimeFixture(t, hostBin, "claude", unstageableRuntimeFixture)
	prependRuntimeFixturePath(t, hostBin)

	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// The seat stages its credential from this directory. It has to exist, or
	// setup fails on the credential before it ever reaches the capability
	// question and the test would prove nothing.
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"fixture-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "gm-review-opus", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "gm-review-opus", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})
	return store, home, checkout
}

// TestSeatRuntimeUnavailableEndsARealAdapterJobBlockedNotFailed is the
// worker-route arm, and `blocked` rather than `failed` is the point of it.
//
// All ten measured instances recorded `failed`, which reads as "this reviewer
// tried and something went wrong" and invites the identical re-dispatch. A
// capability refusal is "nothing here can run", which is what #1817's
// acceptance names.
func TestSeatRuntimeUnavailableEndsARealAdapterJobBlockedNotFailed(t *testing.T) {
	ctx := context.Background()
	store, home, checkout := seedUnavailableRuntimeSeatJob(t, "seat-runtime-unavailable")

	// NO AdapterFactory override: this worker carries the production factory, so
	// it is the one that would actually exec the runtime.
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	job, err := store.GetJob(ctx, "seat-runtime-unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}

	settled, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want %q: a reviewer whose runtime is published unavailable must come back blocked with the failing check named, not as a plain failure", settled.State, workflow.JobBlocked)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var refusal string
	for _, event := range events {
		if event.Kind == seatRuntimeUnavailableEvent {
			refusal = event.Message
		}
	}
	if refusal == "" {
		t.Fatalf("no %q event: the failing check is not readable from the job's own event stream. events=%+v", seatRuntimeUnavailableEvent, events)
	}
	// Each field earns its assertion: the AGENT is who a coordinator must
	// re-dispatch away from, the RUNTIME is the capability that is absent, and
	// the CAUSE is what an operator repairs.
	for _, want := range []string{"gm-review-opus", runtime.ClaudeRuntime, "absent-interpreter-1817"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("%q event %q does not name %q", seatRuntimeUnavailableEvent, refusal, want)
		}
	}
}

// TestSeatRuntimeUnavailableDoesNotRefuseAnInjectedAdapter is the
// over-rejection control, and it is here because the unsafe direction is
// refusal.
//
// An injected adapter never execs the runtime's declared binary, so an absent
// one is not its precondition. A refusal keyed on absence alone rather than on
// the real-adapter discriminator refused work that would have run: measured, it
// failed five existing tests on this host, and it would refuse every claude
// seat in CI, where no runtime CLI is installed at all.
func TestSeatRuntimeUnavailableDoesNotRefuseAnInjectedAdapter(t *testing.T) {
	ctx := context.Background()
	store, home, checkout := seedUnavailableRuntimeSeatJob(t, "seat-runtime-injected")

	runner := &repairStateRunner{}
	launched := false
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	// Assigning AdapterFactory directly withdraws the real-adapter declaration,
	// which is exactly what ~22 fixtures in this package already do.
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		launched = true
		return runtime.ClaudeAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "seat-runtime-injected")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}

	if !launched {
		t.Fatal("an injected adapter was refused for a runtime it was never going to exec")
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == seatRuntimeUnavailableEvent {
			t.Fatalf("injected dispatch recorded a capability refusal: %q", event.Message)
		}
	}
}

// TestSeatRuntimeUnavailableIgnoresAnUnrelatedPublishedRoot pins the
// predicate's reading of a staging result, because the fact it keys on is a
// PATH SHAPE and a substring match on the runtime name would accept the wrong
// root.
func TestSeatRuntimeUnavailableIgnoresAnUnrelatedPublishedRoot(t *testing.T) {
	const root = "/home/.gitmoot/runtimes"
	staged := []string{
		filepath.Join(root, "claude-ba7128fd22b1bc20", ".bin", "claude"),
		filepath.Join(root, "kimi"+toolchain.UnavailableRuntimeSuffix, ".bin", "kimi"),
	}
	if cause := seatRuntimeUnavailable(runtime.ClaudeRuntime, staged, nil); cause != "" {
		t.Errorf("a content-addressed claude root was read as unavailable: %q", cause)
	}
	if cause := seatRuntimeUnavailable(runtime.KimiRuntime, staged, nil); cause == "" {
		t.Errorf("a published-unavailable kimi root was read as available")
	}
	// An absent executable produces no staging diagnostic at all, so the shim
	// alone has to be enough of a fact, and it must not borrow another
	// runtime's diagnostic.
	cause := seatRuntimeUnavailable(runtime.KimiRuntime, staged, []string{"runtime codex could not be staged: unrelated"})
	if !strings.Contains(cause, "kimi") || strings.Contains(cause, "codex") {
		t.Errorf("cause %q borrowed another runtime's diagnostic or lost the runtime name", cause)
	}
}

// unstageableGoFixture is a `go` whose PATH entry is not inside a bin/ or
// sbin/ installation, so no installation can be derived from it.
//
// IT NO LONGER MAKES THE TOOLCHAIN UNAVAILABLE BY ITSELF (#2143), and that is
// the fix working rather than a fixture defect. This fixture is PREPENDED to
// the host PATH, so it used to win purely because exec.LookPath stops at the
// first match. Selection now considers every entry, so a host with a real Go
// further down PATH gets that one - which is exactly the defect #2143
// removed: a junk entry in front must not deny a seat a toolchain that is
// installed.
//
// The unavailability is therefore forced by the WORKSPACE instead, below, so
// the test states its own precondition rather than borrowing PATH order from
// the host.
const unstageableGoFixture = "#!/bin/sh\nexit 0\n"

// seedUnavailableToolchainReviewSeat is the measured #1817 shape that the
// runtime arm cannot see: the seat's OWN runtime stages fine, so
// runtimeUnavailable stays empty, while `go` resolves to the engine's exit-126
// command. That is the live instance, job
// local-review-gm-review-opus-18d3b8e6790b3231-1: gitmoot/gitmoot#2066
// reviewed at its exact head, decision APPROVED, whole tests_run reading
// "could not run ... `go` is a stub that exits 126".
func seedUnavailableToolchainReviewSeat(t *testing.T, jobID string, action string) (*db.Store, string, string) {
	t.Helper()
	return seedToolchainReviewSeat(t, jobID, action, true)
}

func seedToolchainReviewSeat(t *testing.T, jobID string, action string, goModule bool) (*db.Store, string, string) {
	t.Helper()
	hostBin := t.TempDir()
	// The POSITIVE CONTROL rides inside the fixture: claude stages, so a
	// failure here can only come from the toolchain predicate.
	writeRuntimeFixture(t, hostBin, "claude", stageableRuntimeFixture)
	writeRuntimeFixture(t, hostBin, "go", unstageableGoFixture)
	prependRuntimeFixturePath(t, hostBin)

	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	// FORCE THE PRECONDITION IN THE WORKSPACE, not in PATH order (#2143).
	// A module requiring a Go release no host has cannot be satisfied by any
	// installation, here or on a runner, so the seat's toolchain is genuinely
	// unavailable and stays that way as hosts change.
	//
	// COMMITTED, not merely written: the seat reviews an allocated read-only
	// git worktree of this repository, where an untracked file does not
	// exist. An earlier attempt only wrote it, the preflight never fired, and
	// the workspace the seat actually staged against had no go.mod at all.
	if goModule {
		if err := os.WriteFile(filepath.Join(checkout, "go.mod"),
			[]byte("module fixture\n\ngo 99.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, checkout, "add", "go.mod")
		runGit(t, checkout, "commit", "-m", "require a Go release no host has")
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"fixture-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "gm-review-opus", runtime.ClaudeRuntime,
		"550e8400-e29b-41d4-a716-446655440000", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "gm-review-opus", Action: action, Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, RuntimeConfigDir: sourceDir,
	})
	return store, home, checkout
}

func runUnavailableToolchainSeatJob(t *testing.T, store *db.Store, home string, checkout string, jobID string) []db.JobEvent {
	t.Helper()
	ctx := context.Background()
	// NO AdapterFactory override: the production factory is the caller that
	// would actually exec `go` inside the seat.
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// TestSeatToolchainUnavailableEndsAReviewBlockedNotVerdicted is the arm that
// fails before the fix: the job ran to a verdict with nothing executed, and no
// event named the missing capability.
func TestSeatToolchainUnavailableEndsAReviewBlockedNotVerdicted(t *testing.T) {
	t.Setenv("GOWORK", "off")
	ctx := context.Background()
	store, home, checkout := seedUnavailableToolchainReviewSeat(t, "seat-toolchain-unavailable", "review")
	events := runUnavailableToolchainSeatJob(t, store, home, checkout, "seat-toolchain-unavailable")

	settled, err := store.GetJob(ctx, "seat-toolchain-unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != string(workflow.JobBlocked) {
		t.Fatalf("job state = %q, want %q: a reviewer that cannot run `go` must come back blocked with the failing check named, never with a verdict a consumer reads as review evidence", settled.State, workflow.JobBlocked)
	}
	var refusal string
	for _, event := range events {
		if event.Kind == seatToolchainUnavailableEvent {
			refusal = event.Message
		}
		// The sibling arm must stay silent, or this test would pass on the
		// wrong predicate: the fixture's claude stages successfully.
		if event.Kind == seatRuntimeUnavailableEvent {
			t.Fatalf("%q fired on a seat whose own runtime staged: %s", seatRuntimeUnavailableEvent, event.Message)
		}
	}
	if refusal == "" {
		t.Fatalf("no %q event: the failing check is not readable from the job's own event stream. events=%+v", seatToolchainUnavailableEvent, events)
	}
	// The AGENT is who a coordinator re-dispatches away from, and the CAUSE is
	// what an operator repairs.
	// The AGENT a coordinator re-dispatches away from, the REQUIREMENT that
	// could not be met, and the tree that was rejected for it. The last two
	// replace the old "bin/ or sbin/" substring, which named a cause that no
	// longer denies a toolchain on its own.
	for _, want := range []string{"gm-review-opus", "go99.0"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("%q event %q does not name %q", seatToolchainUnavailableEvent, refusal, want)
		}
	}
}

// TestSeatToolchainUnavailableDoesNotRefuseANonReviewSeat is the
// over-rejection control, and it defends a DOCUMENTED choice rather than a
// preference: the staging miss is deliberately un-evented because a job's
// event sequence must not depend on host disk state, and
// TestExecBackendLocalDefaultDaemonE2E pins an exact baseline for an `ask`
// job. Only review's contract requires executing the repository's gate.
func TestSeatToolchainUnavailableDoesNotRefuseANonReviewSeat(t *testing.T) {
	store, home, checkout := seedUnavailableToolchainReviewSeat(t, "seat-toolchain-ask", "ask")
	events := runUnavailableToolchainSeatJob(t, store, home, checkout, "seat-toolchain-ask")

	for _, event := range events {
		if event.Kind == seatToolchainUnavailableEvent {
			t.Fatalf("%q fired on an %q job: the un-evented staging miss is deliberate for every action whose contract does not require running the gate. message=%s", seatToolchainUnavailableEvent, "ask", event.Message)
		}
	}
}

// #2513 (deimos-forge): customer images ship no Go, and every review stopped
// at seat_toolchain_unavailable even for repositories that are not Go. On a
// host with no Go at all, a review of a checkout without go.mod or go.work
// must not be refused for the toolchain; the Go-module case above still is.
func TestSeatToolchainUnavailableDoesNotRefuseANonGoReview(t *testing.T) {
	t.Setenv("GOWORK", "off")
	store, home, checkout := seedToolchainReviewSeat(t, "seat-toolchain-non-go", "review", false)
	// No Go anywhere: only the fixture dir (claude, unstageable go) and the
	// system dirs git needs.
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil && !strings.HasPrefix(dir, os.TempDir()) {
			continue
		}
		kept = append(kept, dir)
	}
	t.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))
	events := runUnavailableToolchainSeatJob(t, store, home, checkout, "seat-toolchain-non-go")
	for _, event := range events {
		if event.Kind == seatToolchainUnavailableEvent {
			t.Fatalf("%q fired for a review of a repository with no go.mod: %s", seatToolchainUnavailableEvent, event.Message)
		}
	}
}

// #2514 review: Go detection must see every tracked module, whatever the
// directory it sits in, and a parent go.work that Go uses even when it has no
// go directive; a tree with neither is not Go.
func TestCheckoutUsesGoFindsEveryModuleAndParentWorkspace(t *testing.T) {
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOWORK", "")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root, checkout string)
		want  bool
	}{
		{"no Go files", func(t *testing.T, _, checkout string) { write(t, filepath.Join(checkout, "package.json"), "{}\n") }, false},
		{"root go.mod", func(t *testing.T, _, checkout string) { write(t, filepath.Join(checkout, "go.mod"), "module x\n") }, true},
		{"go.mod under node_modules", func(t *testing.T, _, checkout string) {
			write(t, filepath.Join(checkout, "node_modules", "backend", "go.mod"), "module x\n")
		}, true},
		{"go.mod under .venv", func(t *testing.T, _, checkout string) {
			write(t, filepath.Join(checkout, ".venv", "backend", "go.mod"), "module x\n")
		}, true},
		{"parent go.work without go directive", func(t *testing.T, root, _ string) {
			write(t, filepath.Join(root, "go.work"), "use ./go-project\n")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			checkout := filepath.Join(root, "checkout")
			if err := os.MkdirAll(checkout, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, root, checkout)
			if got := checkoutUsesGo(checkout); got != tc.want {
				t.Fatalf("checkoutUsesGo = %t, want %t", got, tc.want)
			}
		})
	}
}
