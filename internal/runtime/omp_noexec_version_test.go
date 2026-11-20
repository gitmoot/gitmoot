package runtime

import (
	"strings"
	"testing"
)

// A no-execution seat runs only on omp builds whose gating was measured
// (#2364, #2561, #2571, #2663, 19.0.1); anything else, including a neighbouring patch release or a
// missing probe, must be refused before omp starts.
func TestOmpNoExecutionVersionGate(t *testing.T) {
	for _, version := range []string{"omp/18.10.4", "omp/18.18.6", "omp/18.18.8", " omp/18.18.8\n", "omp/18.20.0", "omp/19.0.1"} {
		if err := OmpNoExecutionVersionError(version); err != nil {
			t.Errorf("measured version %q refused: %v", version, err)
		}
	}
	for _, version := range []string{"", "omp/18.18.5", "omp/18.18.7", "omp/18.18.9", "omp/18.10.40", "18.18.8", "omp/18.18.8-dev", "omp/18.20.1", "omp/19.0.0", "omp/19.0.2"} {
		err := OmpNoExecutionVersionError(version)
		if err == nil {
			t.Errorf("unmeasured version %q accepted", version)
			continue
		}
		// The refusal names every version the gate accepts, so it cannot
		// contradict the allowlist.
		if !strings.Contains(err.Error(), "verified: omp/18.10.4, omp/18.18.6, omp/18.18.8, omp/18.20.0, omp/19.0.1)") {
			t.Errorf("refusal for %q does not list the verified versions: %v", version, err)
		}
	}
}
