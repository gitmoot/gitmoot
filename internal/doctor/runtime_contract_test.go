package doctor

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestCheckRuntimeContractPreservesTriState(t *testing.T) {
	tests := []struct {
		state      runtime.RuntimeContractState
		wantOK     bool
		wantDetail string
	}{
		{runtime.RuntimeContractSupported, true, "supported"},
		{runtime.RuntimeContractUnsupported, false, "contract-violated"},
		{runtime.RuntimeContractUnknown, false, "unknown"},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			check := CheckRuntimeContract(runtime.RuntimeContractResult{
				Runtime: "stub", Version: "1.2.3", State: test.state, Instrument: "binary-help",
				Requirements: []runtime.RuntimeRequirementResult{{Name: "flag --required", Source: "stub::args", State: test.state}},
			})
			if check.OK != test.wantOK || !strings.Contains(check.Detail, test.wantDetail) {
				t.Fatalf("check = %+v, want OK=%v detail containing %q", check, test.wantOK, test.wantDetail)
			}
		})
	}
}
