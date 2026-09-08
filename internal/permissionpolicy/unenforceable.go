package permissionpolicy

import (
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1721. A declared autonomy policy that the target runtime cannot apply is
// recorded and then not enforced: the adapter honestly declares `not-applied`,
// RecordWarning writes it down, and the job runs anyway. Measured over this
// store: 18 such events on omp jobs, every one property=not-applied.
//
// WHAT THIS REFUSES, AND WHY IT IS NOT THE GENERAL PREDICATE THE ISSUE ASKS FOR.
// The obvious rule would be "refuse any policy more restrictive than the runtime
// can apply". That rule is not expressible today, and saying so is more useful
// than approximating it: an adapter declares only whether it applied something
// (applied / widened / not-applied / unresolved), never WHICH policies it could
// have applied. omp declares not-applied for all three, so the general rule
// would refuse all 18 - including 15 that declared danger-full-access, where
// nothing was being restricted and nothing is lost.
//
// So this refuses exactly one case, on its own merits rather than as an
// approximation:
//
//	READ-ONLY IS THE ONE POLICY WHOSE SILENT NON-ENFORCEMENT IS NEVER ACCEPTABLE.
//
// A read-only policy is not a preference, it is the boundary a review seat is
// built on: #1817's whole subject is a reviewer that must not be able to change
// what it reviews. A read-only seat whose sandbox came from "host runtime config
// gitmoot does not read" is a reviewer with unknown write access, and the record
// says it was restricted. workspace-write and danger-full-access are truthfulness
// defects in the record; read-only is a safety boundary that is not there.
//
// MEASURED SCOPE on this store: 1 of the 18 events. The other 17 keep running.
//
// A runtime that gains a real read-only mapping needs no change here: it will
// declare `applied` and this predicate stops matching. Measured against
// omp/18.1.14, which has --approval-mode=(always-ask|write|yolo),
// --auto-approve, --add-dir, --plan-yolo and --plan-yolo-into: `write` maps
// workspace-write, `yolo` maps danger-full-access, and NOTHING maps read-only.
// --plan-yolo is a delayed write - it auto-approves and switches to implement on
// another model - and always-ask in a headless session is a prompt nobody
// answers, so it hangs or auto-denies. Both are failure modes rather than a
// policy, which is why the absence is defended here and not merely recorded.
const readOnlyPolicy = "read-only"

// UnenforceablePolicyRefusal reports whether a job must be refused because the
// agent declared a read-only policy that its runtime declares it did not apply,
// and returns the reason.
//
// It asks the ADAPTER rather than consulting a runtime-name roster, which is the
// design ResolvePermissionPolicyApplication states: a future adapter that gains
// a mapping changes its declaration and every consumer follows.
func UnenforceablePolicyRefusal(agent runtime.Agent, adapter any) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(agent.AutonomyPolicy), readOnlyPolicy) {
		return "", false
	}
	switch Resolve(adapter, agent) {
	case runtime.PermissionPolicyApplied, runtime.PermissionPolicyWidened:
		// Widened is deliberately accepted: the adapter applied SOMETHING and said
		// it was broader than asked. That is a recorded discrepancy for the
		// existing warning to carry, not an absent boundary.
		return "", false
	}
	return fmt.Sprintf(
		"agent %q declares the %s autonomy policy and runtime %q reports it applied no permission-policy flag for this job, "+
			"so the read-only boundary would exist only in the record. This is refused rather than run: a read-only seat whose "+
			"sandbox came from host runtime configuration gitmoot does not read is a reviewer with unknown write access. "+
			"Remedy: dispatch to a runtime that can apply read-only, or change the agent's policy to one the runtime honours",
		agent.Name, readOnlyPolicy, agent.Runtime), true
}
