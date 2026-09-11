package cli

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

// warmSeatStaging materialises a home's staged toolchain and runtime artifacts
// BEFORE a test's measured window, so the window contains no first-time copy.
//
// WHY THIS EXISTS, and why it is a ready barrier rather than a bigger timeout
// (ruling 122694). Read-only seat setup copies the pinned Go toolchain - 15,016
// files, 221.6 MB, ~3.6s on this host - plus one artifact per runtime class, and
// content-addressed staging pays that ONCE PER HOME. A production home is
// long-lived, so the cost lands on the first seat and never again. A test that
// creates a fresh home per run pays it inside whatever window it is measuring,
// and concurrency tests then read "peer is slow to arrive" as "peer serialized".
//
// Measured: TestTrackedPoolIsolationHonorsSamePassRuntimeSibling went 2 pass /
// 1 fail at 4eb38221 and 0/3 at 674f2dd4 purely from staging cost growing by
// ~1.7s; TestReadOnlyWorktreeConcurrentAsksE2E fails even at base e7a3863d on a
// loaded box. Widening their limits would have hidden the property they exist to
// prove - that a seat's setup does not hold a sibling back - so the fix is to
// take the one-time cost OUT of the window instead of making the window bigger.
//
// It is deliberately best-effort: a host with no pinned toolchain or no runtime
// installed stages nothing, and these tests must still run there. Warming
// cannot change an outcome, only when the copying happens.
func warmSeatStaging(t *testing.T, home string) {
	t.Helper()
	paths := config.PathsForHome(home)
	if _, _, diagnostic, err := stageSeatToolchain(paths); err != nil {
		t.Logf("warmSeatStaging: toolchain not pre-staged (%v); the test window will pay the copy", err)
	} else if diagnostic != "" {
		t.Logf("warmSeatStaging: toolchain diagnostic: %s", diagnostic)
	}
	if _, _, diagnostics, err := stageSeatRuntimes(paths); err != nil {
		t.Logf("warmSeatStaging: runtimes not pre-staged (%v); the test window will pay the copy", err)
	} else {
		for _, diagnostic := range diagnostics {
			t.Logf("warmSeatStaging: runtime diagnostic: %s", diagnostic)
		}
	}
}
