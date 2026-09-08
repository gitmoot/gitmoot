//go:build e2e

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1721 P1-1, from PR #2043's review. The refusal existed only on the daemon
// claim path, so a FOREGROUND dispatch still executed a read-only policy that
// nothing applies. The fix added a second call site; this exercises it end to
// end rather than trusting that the call is present.
//
// The instrument that makes it a regression rather than an exit-code check: a
// MARKER the runtime script touches. If the refusal works the script never runs,
// so this fails when the foreground call site is removed and cannot pass merely
// because the command exited non-zero for another reason.
//
// SCOPE, stated because the sibling arm is not here: ReadOnlySeat is not a
// stored agent field. It is derived at dispatch from whether Gitmoot allocated a
// read-only worktree (agent_dispatch.go:605), so the "confined seat still runs"
// arm cannot be staged from the store and is covered at the predicate level in
// internal/permissionpolicy instead.
//
// Deterministic, NO-LLM, offline: the shell runtime declares it applies no
// permission-policy flag, which is exactly the input the predicate refuses.
func TestForegroundDispatchRefusesAnUnenforceableReadOnlyPolicyE2E(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "runtime-must-not-run")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	agent, err := store.GetAgent(ctx, "shell-asker")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	agent.AutonomyPolicy = "read-only"
	if err := store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "work that must not run unconfined",
		"--home", home,
		"--repo", "owner/repo",
	}, &out, &errBuf)

	combined := out.String() + errBuf.String()
	if code == 0 {
		t.Fatalf("foreground dispatch SUCCEEDED with an unenforceable read-only policy; output=%q", combined)
	}
	for _, want := range []string{"read-only", "shell"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("refusal does not name %q, so the caller cannot act on it: %q", want, combined)
		}
	}
	// THE ASSERTION THAT MAKES THIS A REGRESSION: the runtime never executed.
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the runtime script RAN: the foreground refusal did not happen before delivery")
	}
}
