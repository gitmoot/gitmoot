package cli

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// namedAdapter is the Name() half of runtime.Adapter. buildRuntimeAdapter is
// typed to workflow.DeliveryAdapter (Deliver only), so the constructed value's
// identity is asserted through this narrower interface rather than by naming a
// concrete adapter type per runtime — which would make the walk below a list that
// has to be edited for every new runtime, exactly the failure it exists to catch.
type namedAdapter interface{ Name() string }

// TestRuntimeConstructionSwitchesCoverEverySupportedRuntime is the tripwire for
// the two per-runtime CONSTRUCTION switches: buildRuntimeAdapter
// (daemon_worker.go), which the daemon uses to build a job's delivery adapter,
// and runtimeStartAdapter (agent.go), which `agent start`/`agent restart` uses to
// build the session-starting adapter.
//
// Neither is reachable from runtime.Factory: both re-enumerate the runtimes
// themselves and both end in a `default:` that returns "unsupported runtime".
// Registering a runtime in the Factory is therefore not enough — a runtime that
// SupportedRuntimes advertises but a switch omits is accepted by every preflight
// (runtime.ValidateAgent resolves it through the Factory) and then fails at the
// moment work is dispatched. The walk is over SupportedRuntimes rather than a
// literal list so the next runtime is covered without editing this test.
//
// Neither switch legitimately rejects any dispatchable runtime — shell included:
// it is subscribe-only in the sense of running a command instead of an LLM, but
// both switches build ShellAdapter for it, so there is nothing to exclude here.
func TestRuntimeConstructionSwitchesCoverEverySupportedRuntime(t *testing.T) {
	names := runtime.SupportedRuntimes()
	if len(names) == 0 {
		t.Fatal("SupportedRuntimes is empty; this test would pass vacuously")
	}
	// Non-vacuity: omp (#1428) is the runtime this tripwire was written for, so a
	// walk that silently stopped covering it is a failure, not a pass.
	sawOmp := false
	for _, name := range names {
		if name == runtime.OmpRuntime {
			sawOmp = true
		}
	}
	if !sawOmp {
		t.Fatalf("omp must be among the walked runtimes: %v", names)
	}

	// A throwaway home and checkout: the adapters are constructed, never run, so
	// neither path is read. The home is initialized because buildRuntimeAdapter
	// resolves per-runtime credential curation (and Claude runtime auth) beneath it.
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	checkout := t.TempDir()

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			delivery, err := buildRuntimeAdapter(home, runtime.Agent{Runtime: name}, checkout, nil)
			if err != nil {
				t.Fatalf("buildRuntimeAdapter(%q) returned error: %v; every %q job would fail at dispatch", name, err, name)
			}
			if delivery == nil {
				t.Fatalf("buildRuntimeAdapter(%q) returned a nil adapter", name)
			}
			named, ok := delivery.(namedAdapter)
			if !ok {
				t.Fatalf("buildRuntimeAdapter(%q) returned %T, which has no Name()", name, delivery)
			}
			if got := named.Name(); got != name {
				t.Fatalf("buildRuntimeAdapter(%q) built adapter %T with Name() = %q", name, delivery, got)
			}

			start, err := runtimeStartAdapter(runtime.Factory{}, name, checkout)
			if err != nil {
				t.Fatalf("runtimeStartAdapter(%q) returned error: %v; `agent start` and `agent restart` would fail for %q", name, err, name)
			}
			if start == nil {
				t.Fatalf("runtimeStartAdapter(%q) returned a nil adapter", name)
			}
			if got := start.Name(); got != name {
				t.Fatalf("runtimeStartAdapter(%q) built adapter %T with Name() = %q", name, start, got)
			}
		})
	}
}

// TestSetupRuntimeFlagHelpIsDerivedFromRegistry pins `setup --runtime`'s help to
// the adapter registry. The literal it replaced ("codex, claude, or shell") had
// gone stale long before omp existed, and a stale enumeration here is invisible
// in CI: setup still WORKS for an unlisted runtime, so nothing fails — the flag
// just advertises the wrong set to every operator reading `gitmoot setup --help`.
//
// It reads the usage off the registered flag (via the FlagSet's own defaults
// dump, which is what an operator actually sees) rather than re-deriving the
// string, so replacing the derivation with any literal fails here.
func TestSetupRuntimeFlagHelpIsDerivedFromRegistry(t *testing.T) {
	want := "agent runtime: " + strings.Join(runtime.SupportedRuntimes(), "|")
	if !strings.Contains(want, runtime.OmpRuntime) {
		t.Fatalf("expected usage %q to enumerate omp; SupportedRuntimes = %v", want, runtime.SupportedRuntimes())
	}

	var stdout, stderr strings.Builder
	if code := Run([]string{"setup", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("setup --help exit code = %d, stderr=%s", code, stderr.String())
	}

	// flag.PrintDefaults emits "  -runtime string" followed by the usage on its own
	// indented line.
	lines := strings.Split(stderr.String(), "\n")
	got := ""
	found := false
	for i, line := range lines {
		if strings.TrimSpace(line) != "-runtime string" {
			continue
		}
		if i+1 >= len(lines) {
			t.Fatalf("--runtime flag has no usage line; setup --help was:\n%s", stderr.String())
		}
		got = strings.TrimSpace(lines[i+1])
		found = true
		break
	}
	if !found {
		t.Fatalf("setup --help does not register a --runtime flag:\n%s", stderr.String())
	}
	if got != want {
		t.Fatalf("setup --runtime usage = %q, want %q (derive it from runtime.SupportedRuntimes, never a literal)", got, want)
	}
}
