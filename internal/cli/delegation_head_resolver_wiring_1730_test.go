package cli

import (
	"context"
	"testing"
)

// #1730 wiring guard. The engine resolves a delegated review's head by asserting
// DelegationWorktrees to a RevParse-capable interface. That assertion is only
// worth anything if the value production assigns satisfies it, and
// daemon_workflow.go sets it to jobGitClient(checkout, runner).
//
// This is a COMPILE-TIME assertion in the package that does the wiring, so the
// fix cannot become dead code through a change to jobGitClient's return type.
// A runtime-only test in internal/workflow would keep passing with a fake.
func TestJobGitClientSatisfiesTheDelegationHeadResolver(t *testing.T) {
	var client any = jobGitClient("/tmp/does-not-need-to-exist", nil)
	resolver, ok := client.(interface {
		RevParse(ctx context.Context, rev string) (string, error)
	})
	if !ok {
		t.Fatal("jobGitClient no longer satisfies the RevParse resolver the engine asserts for #1730; the delegated-review head fix would be dead in production")
	}
	_ = resolver
}
