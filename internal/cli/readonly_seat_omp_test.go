package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

func ompBrokerLookup(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := pairs[name]
		return value, ok
	}
}

// TestReadOnlyOmpSeatStagesNoCredentialMaterial is the property the old refusal
// was standing in for (#1817): a read-only omp seat must not be able to read the
// owner's omp credential store.
//
// prepareReadOnlyRuntimeState copies ONLY the files a policy names -
// credentialFile, requiredInputs, optionalInputs - into a state dir it creates
// empty. So the discriminating assertion is that all three are empty, which is
// what makes the staged profile fresh rather than a copy of ~/.omp.
//
// A test that only checked "a policy is returned" would pass with a
// credentialFile added, which is the one mistake that would hand a read-only
// reviewer the owner's provider tokens.
func TestReadOnlyOmpSeatStagesNoCredentialMaterial(t *testing.T) {
	policy, err := readOnlyOmpSeatStatePolicy("/home/owner", ompBrokerLookup(map[string]string{
		ompAuthBrokerURLEnv:   "http://127.0.0.1:8765",
		ompAuthBrokerTokenEnv: "broker-bearer",
	}))
	if err != nil {
		t.Fatalf("broker-configured omp seat refused: %v", err)
	}
	if policy.credentialFile != "" {
		t.Errorf("omp seat policy stages credential file %q; a read-only seat must receive no credential material", policy.credentialFile)
	}
	if len(policy.requiredInputs) != 0 {
		t.Errorf("omp seat policy stages required inputs %v; nothing from the owner's ~/.omp may be copied", policy.requiredInputs)
	}
	if len(policy.optionalInputs) != 0 {
		t.Errorf("omp seat policy stages optional inputs %v; nothing from the owner's ~/.omp may be copied", policy.optionalInputs)
	}
	// omp finds its profile under HOME, and the sandbox supplies
	// HOME=cacheRoot/home. Staging anywhere else leaves omp reading the host
	// profile instead of the seat's.
	if !policy.stateAtCacheRoot {
		t.Error("omp seat policy must stage at the cache root: omp resolves its profile from the sandbox HOME")
	}
	if want := filepath.Join("home", ".omp"); policy.relativeState != want {
		t.Errorf("omp seat state = %q, want %q", policy.relativeState, want)
	}
}

// TestReadOnlyOmpSeatRefusesWithoutBrokerMode pins the fail-closed half. Without
// the broker, omp falls back to its local store, and the seat's fresh profile is
// empty - so the alternative to refusing is an opaque auth failure after the
// seat is built, or a future change that stages the owner's store to "fix" it.
func TestReadOnlyOmpSeatRefusesWithoutBrokerMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs map[string]string
		want  string
	}{
		{
			name:  "neither",
			pairs: map[string]string{},
			want:  "broker mode",
		},
		{
			name:  "token only",
			pairs: map[string]string{ompAuthBrokerTokenEnv: "broker-bearer"},
			want:  ompAuthBrokerURLEnv,
		},
		{
			name:  "url only",
			pairs: map[string]string{ompAuthBrokerURLEnv: "http://127.0.0.1:8765"},
			want:  ompAuthBrokerTokenEnv,
		},
		{
			name:  "blank values are not configuration",
			pairs: map[string]string{ompAuthBrokerURLEnv: "   ", ompAuthBrokerTokenEnv: ""},
			want:  "broker mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readOnlyOmpSeatStatePolicy("/home/owner", ompBrokerLookup(tc.pairs))
			if err == nil {
				t.Fatalf("omp seat accepted %v; broker mode is required", tc.pairs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q; the message has to say which half is missing", err.Error(), tc.want)
			}
		})
	}
}

// TestReadOnlyOmpSeatOverlayCarriesOnlyTheBrokerPair checks what the seat's
// environment receives. A read-only seat builds its env from a curated
// allowlist, so the pair arrives only if handed over - and NOTHING else should
// be, because any provider key in that overlay is the owner's credential
// reaching the seat.
func TestReadOnlyOmpSeatOverlayCarriesOnlyTheBrokerPair(t *testing.T) {
	overlay, err := readOnlyOmpBrokerEnv(ompBrokerLookup(map[string]string{
		ompAuthBrokerURLEnv:   "http://127.0.0.1:8765",
		ompAuthBrokerTokenEnv: "broker-bearer",
		"ANTHROPIC_API_KEY":   "sk-owner-key",
		"OPENAI_API_KEY":      "sk-owner-openai",
	}))
	if err != nil {
		t.Fatalf("broker env: %v", err)
	}
	if len(overlay) != 2 {
		t.Fatalf("overlay = %v, want exactly the two broker variables", overlay)
	}
	joined := strings.Join(overlay, " ")
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "sk-owner"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("overlay leaks %q: %v", forbidden, overlay)
		}
	}
	if !strings.Contains(joined, ompAuthBrokerURLEnv+"=http://127.0.0.1:8765") {
		t.Errorf("overlay does not carry the broker url: %v", overlay)
	}
	if !strings.Contains(joined, ompAuthBrokerTokenEnv+"=broker-bearer") {
		t.Errorf("overlay does not carry the broker token: %v", overlay)
	}
}

// TestReadOnlySeatPolicyForOmpIsReachable drives the production entry point
// rather than the helper, because the defect was that this switch refused omp
// before any policy existed. The arm is what dispatch consults.
func TestReadOnlySeatPolicyForOmpIsReachable(t *testing.T) {
	restore := ompBrokerEnvLookup
	t.Cleanup(func() { ompBrokerEnvLookup = restore })
	ompBrokerEnvLookup = ompBrokerLookup(map[string]string{
		ompAuthBrokerURLEnv:   "http://127.0.0.1:8765",
		ompAuthBrokerTokenEnv: "broker-bearer",
	})
	policy, needsState, err := readOnlySeatStatePolicyForRuntime(runtime.OmpRuntime, "/home/owner", false)
	if err != nil {
		t.Fatalf("readOnlySeatStatePolicyForRuntime(omp) = %v; omp must resolve a policy in broker mode", err)
	}
	if !needsState {
		t.Fatal("omp must need isolated state: without it the seat reads the host profile")
	}
	if policy.credentialFile != "" {
		t.Errorf("production arm stages credential file %q", policy.credentialFile)
	}

	ompBrokerEnvLookup = ompBrokerLookup(map[string]string{})
	if _, _, err := readOnlySeatStatePolicyForRuntime(runtime.OmpRuntime, "/home/owner", false); err == nil {
		t.Fatal("production arm accepted omp with no broker configured")
	}
}
