package runtime

import "testing"

// A no-execution seat runs only on omp builds whose gating was measured
// (#2364, #2561); anything else, including a neighbouring patch release or a
// missing probe, must be refused before omp starts.
func TestOmpNoExecutionVersionGate(t *testing.T) {
	for _, version := range []string{"omp/18.10.4", "omp/18.18.6", " omp/18.18.6\n"} {
		if err := OmpNoExecutionVersionError(version); err != nil {
			t.Errorf("measured version %q refused: %v", version, err)
		}
	}
	for _, version := range []string{"", "omp/18.18.5", "omp/18.18.7", "omp/18.10.40", "18.18.6", "omp/18.18.6-dev"} {
		if err := OmpNoExecutionVersionError(version); err == nil {
			t.Errorf("unmeasured version %q accepted", version)
		}
	}
}
