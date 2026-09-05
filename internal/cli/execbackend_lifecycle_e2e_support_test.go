package cli

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

func executionBackendConfigForTest(t *testing.T, worker jobWorker) config.RemoteExecConfig {
	t.Helper()
	cfg, err := worker.executionBackendConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
