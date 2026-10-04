package ompaddon

import (
	"context"
	"os"
	"testing"
)

// TestLiveCheckAgainstOMP runs the add-on inside a real OMP. It is the only
// test that executes the TypeScript, so set GITMOOT_TEST_OMP to an omp binary
// when changing gitmoot-inbox.ts. It uses a local stand-in model and a
// temporary agent directory; nothing outside the temporary directory changes.
func TestLiveCheckAgainstOMP(t *testing.T) {
	omp := os.Getenv("GITMOOT_TEST_OMP")
	if omp == "" {
		t.Skip("set GITMOOT_TEST_OMP=/path/to/omp to run the add-on in a real OMP")
	}
	steps := LiveCheck(context.Background(), omp)
	want := []string{"session", "probe", "idle-delivery", "deferral", "stale-session", "shutdown"}
	for i, name := range want {
		if i >= len(steps) {
			t.Fatalf("live check stopped before %s: %+v", name, steps)
		}
		if steps[i].Name != name || !steps[i].OK {
			t.Fatalf("live check step %d = %+v, want %s ok", i, steps[i], name)
		}
	}
}
