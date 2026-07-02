package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// sandboxProvisioner materializes a FRESH, clean checkout at a given ref for the
// hard-verifier tier (#474) and returns its directory plus a cleanup func. The
// freshness is the whole point: the verifier commands run against a checkout that
// carries ONLY the merged code (no scratch state from the daemon's working tree),
// so the exit-code verdict cannot be tampered with — slime's "no test-cheating"
// isolation. It is its own narrow seam so the dispatcher is unit-testable with a
// fake provisioner (no real git) and so the real git-clone provisioning is
// swappable.
type sandboxProvisioner interface {
	// Provision creates a fresh checkout at ref and returns its directory and a
	// cleanup func the caller MUST call (idempotent, best-effort). A non-nil error
	// means no sandbox was produced (the caller degrade-skips, no hard row); on
	// error the returned cleanup is nil (nothing to clean).
	Provision(ctx context.Context, ref string) (dir string, cleanup func(), err error)
}

// hardVerifierDispatcher is the concrete workflow.HardVerifierDispatcher (#474): on a
// just-merged implement job it provisions a FRESH sandbox checkout at the merged
// head, runs the operator's configured verifier COMMANDS there (`sh -c <command>`,
// exit 0 == pass), and returns an Outcome{Kind:OutcomeReviewed, HardVerifier:true,
// HardPassed:<all-passed>} for the engine to harvest into the auto-trace run as the
// authoritative EvaluatorScore.Hard. The verdict is fail-closed: it PASSES only when
// EVERY command exits 0. It NEVER mutates the merge and NEVER mutates the real
// checkout: the sandbox is an INDEPENDENT local clone (own git dir, config, refs, gc,
// worktree registry — see cloneSandboxProvisioner), and the commands run with their
// working dir set to it, so a verifier that writes relative paths OR invokes git
// writes INSIDE the throwaway clone (which is then discarded), never the daemon's repo
// checkout. An unprovisionable sandbox, an empty command list, or a command that could
// not be RUN at all (missing toolchain in the bare sandbox) yields ok=false (no hard
// row), never an error, so a merge is never blocked and a setup failure never
// fabricates an authoritative negative.
type hardVerifierDispatcher struct {
	store    *db.Store
	runner   subprocess.Runner
	sandbox  sandboxProvisioner
	commands []string
}

var _ workflow.HardVerifierDispatcher = (*hardVerifierDispatcher)(nil)

// Verify provisions a fresh sandbox at the merged head and runs the configured hard
// verifiers there (#474). ok=false means NO verdict was producible (no commands, no
// runner/provisioner, empty ref, the sandbox could not be provisioned, OR a command
// could not be RUN at all — an exec-layer/setup failure), so the engine writes no hard
// row. It NEVER mutates the merge: it runs read-mostly commands in a throwaway
// checkout and returns a value the engine harvests.
func (d *hardVerifierDispatcher) Verify(ctx context.Context, implementJob db.Job, implementPayload workflow.JobPayload, mergedHead string) (workflow.Outcome, bool, error) {
	if d.runner == nil || d.sandbox == nil || len(d.commands) == 0 {
		return workflow.Outcome{}, false, nil
	}
	ref := strings.TrimSpace(mergedHead)
	if ref == "" {
		// No head to check out: nothing to verify. Degrade-skip.
		return workflow.Outcome{}, false, nil
	}

	dir, cleanup, err := d.sandbox.Provision(ctx, ref)
	if err != nil {
		// Could not provision a FRESH sandbox (the merged head is not present in the
		// base checkout, the clone failed, the checkout lock could not be taken, etc.):
		// degrade-skip rather than run the verifiers against a non-fresh tree or fail the
		// merge. No hard row.
		return workflow.Outcome{}, false, nil
	}
	if cleanup != nil {
		defer cleanup()
	}
	if strings.TrimSpace(dir) == "" {
		// Defensive: a provisioner that returned no error but no directory is unusable.
		return workflow.Outcome{}, false, nil
	}

	results := map[string]float64{}
	details := make([]string, 0, len(d.commands))
	allPassed := true
	for _, raw := range d.commands {
		command := strings.TrimSpace(raw)
		if command == "" {
			continue
		}
		switch d.runVerifier(ctx, dir, command) {
		case verifierPass:
			results[command] = 1.0
			details = append(details, fmt.Sprintf("%s (pass)", command))
		case verifierFail:
			results[command] = 0.0
			allPassed = false
			details = append(details, fmt.Sprintf("%s (FAIL)", command))
		case verifierSkip:
			// The command could not be RUN at all (missing interpreter / command-not-found
			// in the bare sandbox — the sandbox carries NO installed deps/toolchain). This
			// is an environmental/setup failure, NOT a verdict on the merged code, so
			// mapping it to an authoritative Hard=0 would poison the optimizer's training
			// signal for a genuinely-good merge. Degrade-skip the WHOLE run (ok=false, no
			// row): a fail-closed AND verdict cannot be trusted once one command was
			// un-runnable, and skipping is strictly safer than fabricating a negative.
			return workflow.Outcome{}, false, nil
		}
	}

	if len(results) == 0 {
		// Every command was blank (defensive): nothing verifiable. ok=false, no row.
		return workflow.Outcome{}, false, nil
	}

	verdict := "FAIL"
	if allPassed {
		verdict = "pass"
	}
	findings := fmt.Sprintf("Hard verifiers on PR #%d in a fresh sandbox at %s [%s]: %s.",
		implementPayload.PullRequest, shortSandboxRef(ref), verdict, strings.Join(details, ", "))

	return workflow.Outcome{
		Kind:         workflow.OutcomeReviewed,
		HardVerifier: true,
		HardPassed:   allPassed,
		Repo:         implementPayload.Repo,
		PullRequest:  implementPayload.PullRequest,
		HeadSHA:      ref,
		Rubric:       results,
		Findings:     findings,
	}, true, nil
}

// verifierVerdict is the tri-state outcome of one verifier command: it PASSED (exit
// 0), it FAILED (ran and reported a genuine non-zero exit, or was killed by the leg's
// timeout — a wedged/too-slow suite is a real negative), or it must be SKIPPED because
// it could not be RUN at all (the interpreter or command is absent in the bare
// sandbox). Skip is distinct from fail precisely so an environmental/setup problem
// never masquerades as an authoritative Hard=0.
type verifierVerdict int

const (
	verifierPass verifierVerdict = iota
	verifierFail
	verifierSkip
)

// runVerifier runs ONE verifier command in the sandbox dir via `sh -c` and classifies
// the result. It runs with the working directory set to the throwaway SANDBOX, never
// the real checkout, so a command's relative writes land in the sandbox. Exit 0 is a
// PASS. A genuine non-zero exit, or a timeout (the leg's bounded context cancels the
// child — a suite that could not finish is a negative, never a silent pass), is a
// FAIL. An exec-layer failure — the interpreter itself missing, or a POSIX
// command-not-found (exit 127) / not-executable (exit 126) from `sh -c` because the
// bare sandbox has no installed toolchain — is a SKIP: the command never actually ran,
// so it is not a verdict on the code and must not fabricate an authoritative Hard=0.
func (d *hardVerifierDispatcher) runVerifier(ctx context.Context, dir string, command string) verifierVerdict {
	_, err := d.runner.Run(ctx, dir, "sh", "-c", command)
	if err == nil {
		return verifierPass
	}
	// A context timeout/cancellation is a genuine FAIL (a wedged or too-slow suite is a
	// negative signal), never a skip — the tier must not silently pass a suite that
	// could not finish. Check this BEFORE the exec-layer classification because a
	// context-killed child surfaces as a signal ExitError.
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return verifierFail
	}
	if isExecLayerFailure(err) {
		return verifierSkip
	}
	// The command ran and reported a non-zero exit: a genuine failure.
	return verifierFail
}

// isExecLayerFailure reports whether err means the verifier command could not be RUN
// at all (as opposed to running and reporting a non-zero exit). The bare hard-verifier
// sandbox has no installed deps/toolchain, so a missing interpreter or an absent
// command is an environmental/setup failure to degrade-skip, not a code verdict.
func isExecLayerFailure(err error) bool {
	if err == nil {
		return false
	}
	// The interpreter itself (sh) could not be located/started, so the process never
	// ran: os/exec surfaces this as exec.ErrNotFound or an *exec.Error.
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true
	}
	// The interpreter ran but could not invoke the command: POSIX shells return 127
	// when the command is not found and 126 when it is found but not executable — both
	// mean the verifier could not RUN (a bare sandbox missing the toolchain), not that
	// the code failed. A genuine test failure uses a different code (typically 1).
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 126, 127:
			return true
		}
	}
	return false
}

// shortSandboxRef trims a ref to a short form for the findings text. A short-SHA-ish
// ref keeps the reasoning compact; a short ref is returned as-is.
func shortSandboxRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// cloneSandboxProvisioner is the real sandboxProvisioner (#474/#617): it materializes
// the fresh checkout as an INDEPENDENT local clone of the daemon's repo checkout,
// detached at the merged head SHA. It is deliberately NOT a `git worktree add` off the
// daemon checkout: a detached worktree SHARES the base repo's object DB, refs, and
// config, so a verifier that shells out to git (`git config`, `git update-ref`,
// `git gc`, `git worktree prune`) would escape into — and could corrupt — the LIVE
// daemon checkout and its other in-flight worktrees. A `git clone --local` instead
// gives the sandbox its own git directory (hardlinked objects, but independent
// config/refs/gc/worktree registry), so a verifier is contained to the throwaway clone
// while staying single-binary (no E2B / containers / network fetch). The clone READ of
// the base .git is serialized on the shared checkout-mutation lock every other
// worktree/checkout op uses (so it never races a concurrent real job mutating the
// base), and the lock is released BEFORE the long verifier phase runs (the phase
// operates entirely inside the independent clone and touches the base not at all).
// Cleanup is a plain directory removal — there is no base worktree registration to
// unwind, so cleanup never mutates the base .git.
type cloneSandboxProvisioner struct {
	// base is the daemon's repo checkout the sandbox is cloned from.
	base string
	// home is GITMOOT_HOME; when set, sandboxes are rooted under it (alongside
	// delegation worktrees) rather than the OS temp dir, keeping scratch checkouts out
	// of the tracked tree and on the same filesystem as the repo.
	home string
	// runner runs the underlying git commands; ExecRunner in production.
	runner subprocess.Runner
	// store backs the checkout-mutation lock that serializes the clone's base read
	// against concurrent real jobs on the same checkout. When nil (unit/E2E paths with
	// no concurrency), the clone runs unserialized — correct because it is read-only and
	// there is no daemon contending for the base.
	store *db.Store
}

// Provision clones the base checkout into a fresh temp root, checks out ref as a
// detached HEAD, and returns the clone dir + a cleanup that removes the temp root. The
// base READ (the local clone) is serialized on the checkout-mutation lock and the lock
// is released before returning, so the caller's (long) verifier phase holds no lock. A
// clone/checkout failure (e.g. the merged head is not present in the base object DB)
// or a lock-acquisition failure returns an error so the dispatcher degrade-skips.
func (p cloneSandboxProvisioner) Provision(ctx context.Context, ref string) (string, func(), error) {
	root, err := os.MkdirTemp(p.tempRoot(), "gitmoot-hardverify-")
	if err != nil {
		return "", nil, fmt.Errorf("create hard-verifier sandbox root: %w", err)
	}
	// git clone refuses a pre-existing non-empty target, so point it at a not-yet-created
	// child of the temp root.
	sandbox := filepath.Join(root, "wt")
	cleanup := func() {
		// The clone is fully independent from the base (its own git dir), so disposal is
		// just removing the temp root — no `git worktree remove` against the base, hence
		// no base-.git mutation and no lock needed on cleanup.
		_ = os.RemoveAll(root)
	}

	// Serialize the base READ on the SAME checkout-mutation key every other worktree/
	// checkout op uses, so the clone never races a concurrent real job mutating the base
	// .git. The leg waits for the lock (never the real job); the lock is released the
	// moment the clone completes, before the long verifier phase, since that phase runs
	// entirely inside the independent clone. When store is nil there is no daemon
	// contending for the base, so the read-only clone runs unserialized.
	if p.store != nil {
		release, lockErr := workflow.AcquireCheckoutMutationLock(ctx, p.store, p.base, "hard-verify:"+shortSandboxRef(ref), time.Now().UTC())
		if lockErr != nil {
			_ = os.RemoveAll(root)
			return "", nil, fmt.Errorf("serialize hard-verifier sandbox clone at %s: %w", shortSandboxRef(ref), lockErr)
		}
		if release != nil {
			// Release with a cancellation-decoupled context so the lock is always dropped,
			// even if the leg's context was cancelled during the clone.
			defer func() { _ = release(context.WithoutCancel(ctx)) }()
		}
	}

	if err := p.cloneDetached(ctx, sandbox, ref); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("provision hard-verifier sandbox at %s: %w", shortSandboxRef(ref), err)
	}
	return sandbox, cleanup, nil
}

// cloneDetached makes the independent local clone at dest and checks out ref detached,
// then severs the origin remote so a verifier can never fetch/push back against the
// live daemon checkout. Removing origin is best-effort hardening — the clone already
// holds every object it needs (hardlinked wholesale) and checks out by raw SHA, so a
// severed origin never reduces what a verifier can read.
func (p cloneSandboxProvisioner) cloneDetached(ctx context.Context, dest string, ref string) error {
	source := git.Client{Runner: p.runner, Dir: p.base}
	if err := source.CloneLocalNoCheckout(ctx, dest); err != nil {
		return err
	}
	clone := git.Client{Runner: p.runner, Dir: dest}
	if err := clone.CheckoutDetach(ctx, ref); err != nil {
		return err
	}
	_ = clone.RemoveRemote(ctx, "origin")
	return nil
}

// tempRoot returns the parent directory for sandbox temp roots: GITMOOT_HOME when
// set (co-locating scratch checkouts with worktrees on the repo's filesystem), else
// "" so os.MkdirTemp uses the OS temp dir.
func (p cloneSandboxProvisioner) tempRoot() string {
	if home := strings.TrimSpace(p.home); home != "" {
		return home
	}
	return ""
}
