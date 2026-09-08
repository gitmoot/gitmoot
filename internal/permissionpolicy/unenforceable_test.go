package permissionpolicy

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1721. BOTH DIRECTIONS ARE PINNED DELIBERATELY, and the negative arm is the
// one that matters: a later "simplification" to refuse every not-applied policy
// would pass the positive test and kill 17 of the 18 measured cases, 15 of them
// declaring danger-full-access where nothing was being restricted at all.

func TestReadOnlyPolicyIsRefusedWhenTheRuntimeAppliedNothing(t *testing.T) {
	agent := runtime.Agent{Name: "gm-omp-impl", Runtime: "omp", AutonomyPolicy: "read-only"}
	reason, refuse := UnenforceablePolicyRefusal(agent, StaticProvider{Property: runtime.PermissionPolicyNotApplied})
	if !refuse {
		t.Fatal("a read-only policy the runtime declares it did not apply was allowed to run; " +
			"the boundary would exist only in the record")
	}
	// The reason has to name the policy, the runtime and a remedy: an operator
	// reading a blocked job needs to know what to change, not merely that it was
	// refused.
	for _, want := range []string{"read-only", "omp", "gm-omp-impl", "Remedy"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("refusal reason omits %q: %s", want, reason)
		}
	}
}

// THE NEGATIVE ARM. workspace-write has a real mapping on omp/18.1.14
// (--approval-mode=write plus --add-dir), and danger-full-access restricts
// nothing, so neither may be refused here. Refusing them would be the
// guard-blocks-valid-work failure this campaign has hit twice.
func TestOtherPoliciesAreNeverRefusedByThisPredicate(t *testing.T) {
	for _, policy := range []string{"workspace-write", "danger-full-access", "auto", "", "  "} {
		agent := runtime.Agent{Name: "a", Runtime: "omp", AutonomyPolicy: policy}
		if _, refuse := UnenforceablePolicyRefusal(agent, StaticProvider{Property: runtime.PermissionPolicyNotApplied}); refuse {
			t.Fatalf("policy %q was refused; only read-only has no mapping, and refusing the rest "+
				"would block 17 of the 18 measured cases", policy)
		}
	}
}

// A runtime that DOES apply the policy must not be refused - which is also how a
// future omp with a real read-only mapping stops matching without any change
// here, because it will declare `applied`.
func TestAnAppliedOrWidenedPolicyIsNeverRefused(t *testing.T) {
	agent := runtime.Agent{Name: "a", Runtime: "claude", AutonomyPolicy: "read-only"}
	for _, property := range []runtime.PermissionPolicyApplication{
		runtime.PermissionPolicyApplied,
		runtime.PermissionPolicyWidened,
	} {
		if _, refuse := UnenforceablePolicyRefusal(agent, StaticProvider{Property: property}); refuse {
			t.Fatalf("a runtime declaring %q was refused; it applied something, so the boundary exists", property)
		}
	}
}

// Unresolved is refused, and that is the fail-closed direction on purpose: an
// adapter that cannot say whether it applied the policy has not established the
// boundary, and read-only is the one policy where "unknown" is not good enough.
func TestUnresolvedIsRefusedForReadOnly(t *testing.T) {
	agent := runtime.Agent{Name: "a", Runtime: "kimi", AutonomyPolicy: "read-only"}
	if _, refuse := UnenforceablePolicyRefusal(agent, StaticProvider{Property: runtime.PermissionPolicyUnresolved}); !refuse {
		t.Fatal("an unresolved declaration was accepted for a read-only policy; unknown is not a boundary")
	}
	// And an adapter with NO declaration at all resolves to not-applied by
	// contract, so it must refuse too.
	if _, refuse := UnenforceablePolicyRefusal(agent, struct{}{}); !refuse {
		t.Fatal("an adapter with no declaration was accepted for a read-only policy")
	}
}

// Case handling: the stored policy is compared case-insensitively because it is
// a gitmoot-owned enum written by an operator, not a repo name.
func TestReadOnlyMatchIsCaseInsensitive(t *testing.T) {
	for _, spelling := range []string{"read-only", "Read-Only", "READ-ONLY", " read-only "} {
		agent := runtime.Agent{Name: "a", Runtime: "omp", AutonomyPolicy: spelling}
		if _, refuse := UnenforceablePolicyRefusal(agent, StaticProvider{Property: runtime.PermissionPolicyNotApplied}); !refuse {
			t.Fatalf("spelling %q was not recognised as read-only", spelling)
		}
	}
}
