package cli

import (
	"os"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// legacyPipelineConfig is the pipeline-era vocabulary an operator's existing
// config.toml can still carry after #2542 retired pipelines: the
// [pipeline_remote] section, the brain-era [memory.pipelines] section that the
// live home still has, a runtime override advertising the retired "produce"
// capability, and an agent type whose only capability was "produce".
const legacyPipelineConfig = `
[pipeline_remote]
repo = "owner/pipelines"
ref = "main"
path = "pipelines"

[memory.pipelines]
ingest_sweep = "24h"
groom_propose = "24h"

[runtimes.codex]
capabilities = ["review", "implement", "ask", "produce"]

[agents.appshots-writer]
runtime = "codex"
role = "implementer"
capabilities = ["produce"]
autonomy_policy = "workspace-write"
`

// TestConfigWithRetiredPipelineKeysStillLoads pins that an existing config.toml
// keeps loading after the pipeline retirement (#2542). Pipelines left with no
// compatibility shim, so the only acceptable behaviour for their keys is to be
// ignored, never to fail a load.
//
// The runtime override is the load-bearing case: the strict capability
// validator rejects any action outside review/implement/ask, so a naive removal
// of "produce" from the closed set turns every config that ever listed it into
// a hard error on the daemon's runtime-registry path. The validating write path
// (SetConfigScalar re-runs every section parser) and the agent-type loader are
// asserted too, because those are the other two places an operator meets a
// stale key.
func TestConfigWithRetiredPipelineKeysStillLoads(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+legacyPipelineConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	registry, err := resolveRuntimeRegistry(paths)
	if err != nil {
		t.Fatalf("resolveRuntimeRegistry with a legacy produce capability: %v", err)
	}
	codex, ok := registry.Metadata(runtime.CodexRuntime)
	if !ok {
		t.Fatal("codex runtime missing from the resolved registry")
	}
	if want := []string{"review", "implement", "ask"}; !reflect.DeepEqual(codex.Capabilities, want) {
		t.Fatalf("codex capabilities = %v, want %v (the retired produce entry dropped)", codex.Capabilities, want)
	}

	types, err := config.LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes with a legacy produce agent type: %v", err)
	}
	if _, ok := types["appshots-writer"]; !ok {
		t.Fatalf("agent type appshots-writer missing after load: %v", types)
	}

	if err := config.SetConfigScalar(paths, []string{"agents", "appshots-writer", "autonomy_policy"}, config.StringScalar("read-only")); err != nil {
		t.Fatalf("validated config write with legacy pipeline sections present: %v", err)
	}
}
