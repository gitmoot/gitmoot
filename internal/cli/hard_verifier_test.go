package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// fakeSandboxProvisioner is a sandboxProvisioner that returns a canned dir (or an
// error) and records the ref it was asked to provision + whether cleanup ran.
type fakeSandboxProvisioner struct {
	dir         string
	err         error
	provisioned string
	cleaned     bool
}

func (p *fakeSandboxProvisioner) Provision(_ context.Context, ref string) (string, func(), error) {
	p.provisioned = ref
	if p.err != nil {
		return "", nil, p.err
	}
	return p.dir, func() { p.cleaned = true }, nil
}

// fakeVerifierRunner is a subprocess.Runner that returns a canned error per verifier
// command (the `sh -c <command>` argument) and records the working DIR each command
// ran in — so a test can assert the commands ran in the SANDBOX, never the real
// checkout. An optional onRun hook lets a test model a command's side effect.
type fakeVerifierRunner struct {
	errByCommand map[string]error
	onRun        func(dir, command string)
	calls        []verifierCall
}

type verifierCall struct {
	dir     string
	command string
}

func (r *fakeVerifierRunner) Run(_ context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	verifier := ""
	if command == "sh" && len(args) == 2 && args[0] == "-c" {
		verifier = args[1]
	}
	r.calls = append(r.calls, verifierCall{dir: dir, command: verifier})
	if r.onRun != nil {
		r.onRun(dir, verifier)
	}
	return subprocess.Result{}, r.errByCommand[verifier]
}

func (r *fakeVerifierRunner) LookPath(file string) (string, error) { return "/bin/" + file, nil }

var _ subprocess.Runner = (*fakeVerifierRunner)(nil)

func hardVerifierImplementPayload() workflow.JobPayload {
	return workflow.JobPayload{Repo: "owner/repo", PullRequest: 7}
}

// TestHardVerifierAllPassIsPass: every command exits 0 → HardPassed=true, ok=true, a
// per-command rubric of all-1.0, tagged HardVerifier, and the sandbox is cleaned up.
func TestHardVerifierAllPassIsPass(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{dir: "/sandbox/wt"}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{}} // no errors => all pass
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"go build ./...", "go test ./..."},
	}
	outcome, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "deadbeefcafe")
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if !ok {
		t.Fatal("all-pass verifiers must produce a verdict (ok=true)")
	}
	if !outcome.HardVerifier || !outcome.HardPassed {
		t.Fatalf("outcome = HardVerifier=%v HardPassed=%v, want both true", outcome.HardVerifier, outcome.HardPassed)
	}
	if outcome.Kind != workflow.OutcomeReviewed {
		t.Fatalf("outcome kind = %q, want OutcomeReviewed", outcome.Kind)
	}
	if outcome.Rubric["go build ./..."] != 1.0 || outcome.Rubric["go test ./..."] != 1.0 {
		t.Fatalf("rubric = %v, want all 1.0", outcome.Rubric)
	}
	if !prov.cleaned {
		t.Fatal("the sandbox cleanup must run")
	}
	if prov.provisioned != "deadbeefcafe" {
		t.Fatalf("provisioned ref = %q, want the merged head", prov.provisioned)
	}
}

// TestHardVerifierAnyFailIsFail: a SINGLE failing command (fail-closed set membership)
// makes the whole verdict FAIL, and that command's rubric entry is 0.0.
func TestHardVerifierAnyFailIsFail(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{dir: "/sandbox/wt"}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{
		"go test ./...": errors.New("exit status 1"),
	}}
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"go build ./...", "go test ./..."},
	}
	outcome, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "head")
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true err=nil", ok, err)
	}
	if outcome.HardPassed {
		t.Fatal("a single failing command must make the verdict FAIL")
	}
	if outcome.Rubric["go build ./..."] != 1.0 || outcome.Rubric["go test ./..."] != 0.0 {
		t.Fatalf("rubric = %v, want go build=1.0 go test=0.0", outcome.Rubric)
	}
	if !prov.cleaned {
		t.Fatal("the sandbox cleanup must run even on a FAIL verdict")
	}
}

// TestHardVerifierTimeoutIsFail: a command whose Run returns a context-deadline error
// (the leg's bounded context cancelled it) is a FAIL, never a silent pass.
func TestHardVerifierTimeoutIsFail(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{dir: "/sandbox/wt"}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{
		"slow-suite": context.DeadlineExceeded,
	}}
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"slow-suite"},
	}
	outcome, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "head")
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true err=nil", ok, err)
	}
	if outcome.HardPassed {
		t.Fatal("a timed-out (context-cancelled) verifier must FAIL, not pass")
	}
}

// TestHardVerifierRunsInSandboxNotRealCheckout: EVERY verifier command runs with its
// working dir set to the SANDBOX the provisioner returned, never any other checkout.
// This is the isolation contract the tier depends on for cheat-proofing.
func TestHardVerifierRunsInSandboxNotRealCheckout(t *testing.T) {
	t.Parallel()
	const sandboxDir = "/tmp/gitmoot-hardverify-XYZ/wt"
	prov := &fakeSandboxProvisioner{dir: sandboxDir}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{}}
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"cmd-one", "cmd-two"},
	}
	if _, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "head"); err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected two verifier runs, got %d", len(runner.calls))
	}
	for _, call := range runner.calls {
		if call.dir != sandboxDir {
			t.Fatalf("verifier %q ran in %q, want the sandbox %q", call.command, call.dir, sandboxDir)
		}
	}
}

// TestHardVerifierUnprovisionableSandboxSkips: a provisioner error yields ok=false (no
// hard row) and runs NO verifier — the leg degrade-skips, never fails the merge.
func TestHardVerifierUnprovisionableSandboxSkips(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{err: errors.New("merged head not present in base checkout")}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{}}
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"go test ./..."},
	}
	outcome, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "head")
	if err != nil {
		t.Fatalf("Verify must not error on an unprovisionable sandbox, got %v", err)
	}
	if ok {
		t.Fatalf("an unprovisionable sandbox must skip (ok=false), got outcome %+v", outcome)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no verifier must run without a sandbox, got %d calls", len(runner.calls))
	}
}

// TestHardVerifierExecLayerFailureSkips: a command that could not be RUN at all (the
// runner reports exec.ErrNotFound — the bare sandbox has no installed toolchain) must
// degrade-skip the WHOLE run (ok=false, no row), NOT fabricate an authoritative Hard=0
// negative from a setup failure (#474 false-negative hardening). A genuine exit-1 stays
// a FAIL — see TestHardVerifierAnyFailIsFail.
func TestHardVerifierExecLayerFailureSkips(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{dir: "/sandbox/wt"}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{
		// The first command passes; the second cannot be run at all.
		"npm test": exec.ErrNotFound,
	}}
	d := &hardVerifierDispatcher{
		runner:   runner,
		sandbox:  prov,
		commands: []string{"go build ./...", "npm test"},
	}
	outcome, ok, err := d.Verify(context.Background(), db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), "head")
	if err != nil {
		t.Fatalf("Verify must not error on an un-runnable command, got %v", err)
	}
	if ok {
		t.Fatalf("an un-runnable (exec-layer) command must skip the whole run (ok=false), got outcome %+v", outcome)
	}
}

// TestHardVerifierEmptyInputsSkip: no commands, no runner/provisioner, or an empty
// merged head all skip (ok=false) — byte-identical no-op guards.
func TestHardVerifierEmptyInputsSkip(t *testing.T) {
	t.Parallel()
	prov := &fakeSandboxProvisioner{dir: "/sandbox/wt"}
	runner := &fakeVerifierRunner{errByCommand: map[string]error{}}
	cases := []struct {
		name string
		d    *hardVerifierDispatcher
		head string
	}{
		{"no commands", &hardVerifierDispatcher{runner: runner, sandbox: prov}, "head"},
		{"nil runner", &hardVerifierDispatcher{sandbox: prov, commands: []string{"go test ./..."}}, "head"},
		{"nil sandbox", &hardVerifierDispatcher{runner: runner, commands: []string{"go test ./..."}}, "head"},
		{"empty head", &hardVerifierDispatcher{runner: runner, sandbox: prov, commands: []string{"go test ./..."}}, "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok, err := tc.d.Verify(context.Background(), db.Job{ID: "j"}, hardVerifierImplementPayload(), tc.head)
			if err != nil || ok {
				t.Fatalf("%s: Verify = ok %v err %v, want ok=false err=nil", tc.name, ok, err)
			}
		})
	}
}

// --- E2E (deterministic, real git worktree sandbox + real sh) ---

// gitFixtureRepo inits a real git repo with one committed fixture file and returns the
// repo dir + the committed HEAD SHA — the sandbox is provisioned at this SHA.
func gitFixtureRepo(t *testing.T, marker string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "marker.txt"), []byte(marker), 0o600); err != nil {
		t.Fatalf("write fixture marker: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "fixture")
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return repoDir, strings.TrimSpace(string(out))
}

// realHardVerifierDispatcher wires the REAL worktree provisioner + real ExecRunner
// against a fixture repo, so the E2E exercises actual git worktree isolation and
// actual `sh -c` exit codes — no fakes.
func realHardVerifierDispatcher(repoDir string, commands []string) *hardVerifierDispatcher {
	return &hardVerifierDispatcher{
		runner: subprocess.GroupRunner{},
		sandbox: cloneSandboxProvisioner{
			base:   repoDir,
			runner: subprocess.ExecRunner{},
		},
		commands: commands,
	}
}

// installHardVerifierTemplate installs a template and returns its version + an
// implement payload attributed to it, so the real harvester resolves the version.
func installHardVerifierTemplate(t *testing.T, store *db.Store) (db.AgentTemplateVersion, workflow.JobPayload) {
	t.Helper()
	version := installChainTemplate(t, store, "planner")
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		PullRequest:            7,
		TaskID:                 "task-1",
		TemplateID:             version.TemplateID,
		TemplateResolvedCommit: version.ResolvedCommit,
	}
	return version, payload
}

// TestHardVerifierE2EPassFlowsIntoHardScore drives a REAL fixture repo + a REAL
// exit-0 verifier (`test -f marker.txt` — the committed file IS present in the fresh
// checkout) through the REAL harvester, proving a PASS lands a choice "a" hard row
// whose persisted hard_score is 1.0 (EvaluatorScore.Hard=1.0).
func TestHardVerifierE2EPassFlowsIntoHardScore(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir, sha := gitFixtureRepo(t, "hello")
	d := realHardVerifierDispatcher(repoDir, []string{"test -f marker.txt"})

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if !outcome.HardPassed {
		t.Fatal("exit-0 verifier on the fresh checkout must PASS (the committed marker.txt exists)")
	}

	store := openTraceChainStore(t)
	version, payload := installHardVerifierTemplate(t, store)
	h := skillopt.NewOutcomeHarvester(store, nil)
	if err := h.Harvest(ctx, db.Job{ID: "implement-job", Type: "implement"}, payload, outcome); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("expected one choice=a hard row, got %+v", events)
	}
	assertPersistedHardScore(t, store, version.ID, 1.0, true)
}

// TestHardVerifierE2EFailFlowsIntoHardScore drives a REAL exit-1 verifier through the
// REAL harvester, proving a FAIL lands a choice "b" hard row whose persisted
// hard_score is 0.0 (EvaluatorScore.Hard=0.0). This is the mutation guard for the
// exit-code mapping: if pass/fail were inverted, this row would be choice "a".
func TestHardVerifierE2EFailFlowsIntoHardScore(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir, sha := gitFixtureRepo(t, "hello")
	// The file does NOT exist in the checkout → `test -f` exits 1 → FAIL.
	d := realHardVerifierDispatcher(repoDir, []string{"test -f does-not-exist.txt"})

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if outcome.HardPassed {
		t.Fatal("exit-1 verifier must FAIL")
	}

	store := openTraceChainStore(t)
	version, payload := installHardVerifierTemplate(t, store)
	h := skillopt.NewOutcomeHarvester(store, nil)
	if err := h.Harvest(ctx, db.Job{ID: "implement-job", Type: "implement"}, payload, outcome); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 || events[0].Choice != "b" {
		t.Fatalf("expected one choice=b hard row, got %+v", events)
	}
	assertPersistedHardScore(t, store, version.ID, 0.0, false)
}

// TestHardVerifierE2ESandboxIsolatesWrites drives a REAL verifier that WRITES a file
// (escapee.txt) via a relative path. The write must land in the throwaway sandbox
// (which is then cleaned up), NEVER the real base checkout — proving the freshness /
// isolation guarantee. This is the mutation guard for the sandbox-fresh contract: if
// the verifier ran in the base checkout, escapee.txt would appear there.
func TestHardVerifierE2ESandboxIsolatesWrites(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir, sha := gitFixtureRepo(t, "hello")
	// The verifier writes a relative file, then exits 0.
	d := realHardVerifierDispatcher(repoDir, []string{"echo escaped > escapee.txt"})

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if !outcome.HardPassed {
		t.Fatal("the write-then-exit-0 verifier must PASS")
	}
	// The real base checkout MUST be untouched — the write went to the throwaway
	// sandbox, which was force-removed on cleanup.
	if _, err := os.Stat(filepath.Join(repoDir, "escapee.txt")); !os.IsNotExist(err) {
		t.Fatalf("a verifier write leaked into the REAL checkout %q (isolation broken): stat err=%v", repoDir, err)
	}
}

// TestHardVerifierE2ESandboxIsolatesGitState drives a REAL verifier that shells out to
// git — writing config, creating a ref, and running gc/worktree-prune — and asserts
// the base repo's git state (config, refs, and worktree registry) is COMPLETELY
// untouched. This is the containment guard for finding #474: the sandbox is an
// independent local clone (its own git dir), so a git-invoking verifier is confined to
// the throwaway clone and cannot escape into the live daemon checkout. A detached
// worktree off the base (the pre-fix design) would have leaked all three.
func TestHardVerifierE2ESandboxIsolatesGitState(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir, sha := gitFixtureRepo(t, "hello")

	baseRefsBefore := gitShowRef(t, repoDir)

	// The verifier writes git config, creates a branch ref, gc's, and prunes worktrees —
	// all of which, in the sandbox's OWN git dir, must never reach the base repo.
	d := realHardVerifierDispatcher(repoDir, []string{
		"git config hardverify.escaped yes && " +
			"git update-ref refs/heads/hardverify-escaped HEAD && " +
			"git gc --prune=now && git worktree prune",
	})

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if !outcome.HardPassed {
		t.Fatal("the git-mutating verifier chain must exit 0 (PASS) so the base-untouched assertions are meaningful")
	}

	// 1. Config: the escaped key must NOT be in the base repo's config.
	if got := gitConfigGet(t, repoDir, "hardverify.escaped"); got != "" {
		t.Fatalf("verifier `git config` escaped into the base repo (hardverify.escaped=%q): git isolation broken", got)
	}
	// 2. Refs: the base repo's refs must be exactly what they were, with no new branch.
	if after := gitShowRef(t, repoDir); after != baseRefsBefore {
		t.Fatalf("verifier `git update-ref`/`git gc` mutated the base repo's refs:\nbefore:\n%s\nafter:\n%s", baseRefsBefore, after)
	}
	// 3. Worktree registry: the base repo must have registered NO worktree (the sandbox
	// is a clone, not a worktree off the base), so no admin dir leaked.
	if entries := gitWorktreeRegistryEntries(t, repoDir); len(entries) != 0 {
		t.Fatalf("verifier leaked worktree registrations into the base repo: %v", entries)
	}
}

// TestHardVerifierE2ECommandNotFoundSkips drives a REAL verifier whose command is
// absent in the bare sandbox (`sh -c` returns exit 127). The tier must degrade-skip
// (ok=false, no row), never map the missing-toolchain exit onto an authoritative
// Hard=0 — the false-negative hardening (#474) proven end to end through real `sh`.
func TestHardVerifierE2ECommandNotFoundSkips(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir, sha := gitFixtureRepo(t, "hello")
	d := realHardVerifierDispatcher(repoDir, []string{"gitmoot-hardverify-missing-command-xyz --run"})

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil {
		t.Fatalf("Verify must not error on a command-not-found, got %v", err)
	}
	if ok {
		t.Fatalf("a command-not-found (exit 127) must skip (ok=false, no row), got outcome %+v", outcome)
	}
}

// gitShowRef returns the base repo's full ref listing (sorted-by-git `show-ref`
// output), or "" when the repo has no refs. It is the before/after fingerprint for the
// git-state isolation assertion.
func gitShowRef(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "show-ref").Output()
	if err != nil {
		// `git show-ref` exits 1 with no output when there are no refs; treat as empty.
		if _, ok := err.(*exec.ExitError); ok {
			return ""
		}
		t.Fatalf("git show-ref: %v", err)
	}
	return string(out)
}

// gitConfigGet returns a local config value from the base repo, or "" when unset.
func gitConfigGet(t *testing.T, repoDir string, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "config", "--local", "--get", key).Output()
	if err != nil {
		// `git config --get` exits 1 when the key is unset.
		if _, ok := err.(*exec.ExitError); ok {
			return ""
		}
		t.Fatalf("git config --get %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// gitWorktreeRegistryEntries lists the base repo's .git/worktrees admin dirs (the
// registrations a `git worktree add` off the base would create). An empty slice proves
// the sandbox never touched the base worktree registry.
func gitWorktreeRegistryEntries(t *testing.T, repoDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoDir, ".git", "worktrees"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read .git/worktrees: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestHardVerifierE2ETimeoutFailsFromRealContext drives a REAL slow command against a
// short leg context, proving a genuine timeout (not a fake) is a FAIL.
func TestHardVerifierE2ETimeoutFailsFromRealContext(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repoDir, sha := gitFixtureRepo(t, "hello")
	d := realHardVerifierDispatcher(repoDir, []string{"sleep 30"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	outcome, ok, err := d.Verify(ctx, db.Job{ID: "implement-job"}, hardVerifierImplementPayload(), sha)
	if err != nil || !ok {
		t.Fatalf("Verify = ok %v err %v, want ok=true", ok, err)
	}
	if outcome.HardPassed {
		t.Fatal("a real command killed by the leg's context timeout must FAIL")
	}
}

// assertPersistedHardScore reads the hard item's metadata and asserts the persisted
// hard_score + hard_passed — proving the binary verdict flowed into
// EvaluatorScore.Hard end to end (through the real sandbox and the real harvester).
func assertPersistedHardScore(t *testing.T, store *db.Store, versionID string, wantScore float64, wantPassed bool) {
	t.Helper()
	items, err := store.ListEvalReviewItems(context.Background(), "auto-trace:"+versionID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	for _, item := range items {
		if !strings.HasPrefix(item.ItemID, "hard#") {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(item.MetadataJSON), &meta); err != nil {
			t.Fatalf("hard item metadata invalid JSON: %v", err)
		}
		if score, _ := meta["hard_score"].(float64); score != wantScore {
			t.Fatalf("persisted hard_score = %v, want %v", meta["hard_score"], wantScore)
		}
		if passed, _ := meta["hard_passed"].(bool); passed != wantPassed {
			t.Fatalf("persisted hard_passed = %v, want %v", meta["hard_passed"], wantPassed)
		}
		return
	}
	t.Fatalf("no hard# item found for version %s", versionID)
}
