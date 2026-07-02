package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// fake provisioner (no real git) and so the real git-worktree provisioning is
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
// EVERY command exits 0. It NEVER mutates the merge and NEVER touches the real
// checkout — the commands run with their working dir set to the throwaway sandbox, so
// a verifier that writes relative paths writes INSIDE the sandbox (which is then
// discarded), never the daemon's repo checkout. An unprovisionable sandbox or an
// empty command list yields ok=false (no hard row), never an error, so a merge is
// never blocked.
type hardVerifierDispatcher struct {
	store    *db.Store
	runner   subprocess.Runner
	sandbox  sandboxProvisioner
	commands []string
}

var _ workflow.HardVerifierDispatcher = (*hardVerifierDispatcher)(nil)

// Verify provisions a fresh sandbox at the merged head and runs the configured hard
// verifiers there (#474). ok=false means NO verdict was producible (no commands, no
// runner/provisioner, empty ref, or the sandbox could not be provisioned), so the
// engine writes no hard row. It NEVER mutates the merge: it runs read-mostly
// commands in a throwaway checkout and returns a value the engine harvests.
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
		// base checkout, a worktree add failed, etc.): degrade-skip rather than run the
		// verifiers against a non-fresh tree or fail the merge. No hard row.
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
		if d.runVerifier(ctx, dir, command) {
			results[command] = 1.0
			details = append(details, fmt.Sprintf("%s (pass)", command))
			continue
		}
		results[command] = 0.0
		allPassed = false
		details = append(details, fmt.Sprintf("%s (FAIL)", command))
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

// runVerifier runs ONE verifier command in the sandbox dir via `sh -c` and reports
// whether it exited 0 (pass). It runs with the working directory set to the
// throwaway SANDBOX, never the real checkout, so a command's relative writes land in
// the sandbox. Any non-zero exit — a real failure, a context timeout (the leg's
// bounded context cancels the child), or `sh` itself being unavailable — is a FAIL,
// never a skip: an un-runnable verifier is a negative signal, not a silent pass (the
// tier exists precisely to REFUSE to fabricate a positive without proof).
func (d *hardVerifierDispatcher) runVerifier(ctx context.Context, dir string, command string) bool {
	_, err := d.runner.Run(ctx, dir, "sh", "-c", command)
	return err == nil
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

// worktreeSandboxProvisioner is the real sandboxProvisioner (#474): it materializes
// the fresh checkout as a DETACHED git worktree off the daemon's repo checkout at the
// merged head SHA — reusing gitmoot's existing worktree isolation (internal/git),
// NOT E2B / containers / a second clone, so the single-binary moat is preserved. The
// worktree is a clean tree at exactly the merged head; the cleanup force-removes the
// worktree (so the base repo's worktree registration does not leak) and deletes the
// temp root.
type worktreeSandboxProvisioner struct {
	// base is the daemon's repo checkout the detached worktree is added off of.
	base string
	// home is GITMOOT_HOME; when set, sandboxes are rooted under it (alongside
	// delegation worktrees) rather than the OS temp dir, keeping scratch checkouts out
	// of the tracked tree and on the same filesystem as the repo.
	home string
	// runner runs the underlying git commands; ExecRunner in production.
	runner subprocess.Runner
}

// Provision adds a detached worktree at ref under a fresh temp root and returns the
// worktree dir + a cleanup that force-removes the worktree and the temp root. A
// worktree-add failure (e.g. the merged head is not present in the base checkout)
// returns an error so the dispatcher degrade-skips.
func (p worktreeSandboxProvisioner) Provision(ctx context.Context, ref string) (string, func(), error) {
	root, err := os.MkdirTemp(p.tempRoot(), "gitmoot-hardverify-")
	if err != nil {
		return "", nil, fmt.Errorf("create hard-verifier sandbox root: %w", err)
	}
	// git worktree add refuses a pre-existing target, so point it at a not-yet-created
	// child of the temp root.
	worktree := filepath.Join(root, "wt")
	client := git.Client{Runner: p.runner, Dir: p.base}
	cleanup := func() {
		// Force-remove the worktree registration first (best-effort), then the root, so
		// the base repo does not accumulate stale .git/worktrees entries. Use a
		// cancellation-decoupled context so cleanup runs even after the leg's context is
		// cancelled/timed out.
		_ = client.RemoveWorktreeForce(context.WithoutCancel(ctx), worktree)
		_ = os.RemoveAll(root)
	}
	if err := client.AddDetachedWorktree(ctx, worktree, ref); err != nil {
		// No worktree was registered; just drop the temp root.
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("provision hard-verifier sandbox at %s: %w", shortSandboxRef(ref), err)
	}
	return worktree, cleanup, nil
}

// tempRoot returns the parent directory for sandbox temp roots: GITMOOT_HOME when
// set (co-locating scratch checkouts with worktrees on the repo's filesystem), else
// "" so os.MkdirTemp uses the OS temp dir.
func (p worktreeSandboxProvisioner) tempRoot() string {
	if home := strings.TrimSpace(p.home); home != "" {
		return home
	}
	return ""
}
