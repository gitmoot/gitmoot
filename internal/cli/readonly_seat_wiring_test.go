package cli

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// redactEnvNames returns NAMES only, so a failure message never prints a token.
func redactEnvNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	return names
}

// Every runner shape used by the daemon must receive the same curated BaseEnv.
// In particular, the default CuratedGroupRunner and execbackend InstanceRunner
// must both drop ambient GitHub credentials.
func TestWrapReadOnlySandboxAdapterCuratesEveryRunnerShape(t *testing.T) {
	home := t.TempDir()
	readOnlySeatOmpBrokerEnv(t)
	// An ambient GH_TOKEN must never reach a read-only seat: gh prefers it over
	// the redirected GH_CONFIG_DIR, so inheriting it hands a review a live
	// GitHub credential.
	t.Setenv("GH_TOKEN", "ambient-github-token")
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	agent := runtime.Agent{Runtime: runtime.OmpRuntime, ReadOnlySeat: true}

	for name, runner := range map[string]subprocess.Runner{
		"nil runner":                    nil,
		"group runner":                  subprocess.GroupRunner{},
		"curated group runner":          subprocess.CuratedGroupRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}},
		"execbackend instance runner":   execbackend.InstanceRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}},
		"tee over curated group runner": subprocess.TeeRunner{Inner: subprocess.CuratedGroupRunner{BaseEnv: []string{"GH_TOKEN=ambient-github-token"}}},
		"env injecting over group":      subprocess.EnvInjectingRunner{Inner: subprocess.GroupRunner{}},
	} {
		t.Run(name, func(t *testing.T) {
			wrapped, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout, "", runtime.OmpAdapter{Runner: runner})
			if err != nil {
				t.Fatalf("wrapReadOnlySandboxAdapter: %v", err)
			}
			seat, ok := wrapped.(readOnlyRuntimeAdapter)
			if !ok {
				t.Fatalf("wrapped adapter %T, want readOnlyRuntimeAdapter", wrapped)
			}
			t.Cleanup(func() { _ = seat.cleanup() })
			inner, ok := seat.Adapter.(runtime.OmpAdapter)
			if !ok {
				t.Fatalf("inner adapter %T, want runtime.OmpAdapter", seat.Adapter)
			}
			_, baseEnv := seatRunnerEnvironments(t, inner.Runner)
			for _, entry := range baseEnv {
				if strings.HasPrefix(entry, "GH_TOKEN=") {
					t.Fatalf("seat base env inherited GH_TOKEN: %v", redactEnvNames(baseEnv))
				}
			}
		})
	}
}

// seatRunnerEnvironments returns the sandbox wrapper's env and the innermost
// runner's base env, so a test can tell the grants route from the graft route
// apart instead of passing on either alone.
func seatRunnerEnvironments(t *testing.T, runner subprocess.Runner) (sandboxEnv []string, baseEnv []string) {
	t.Helper()
	for {
		switch r := runner.(type) {
		case subprocess.WrappingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case *subprocess.WrappingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case subprocess.TeeRunner:
			runner = r.Inner
		case *subprocess.TeeRunner:
			runner = r.Inner
		case subprocess.EnvInjectingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case *subprocess.EnvInjectingRunner:
			sandboxEnv = append(sandboxEnv, r.Env...)
			runner = r.Inner
		case subprocess.CuratedGroupRunner:
			return sandboxEnv, r.BaseEnv
		case *subprocess.CuratedGroupRunner:
			return sandboxEnv, r.BaseEnv
		case execbackend.InstanceRunner:
			return sandboxEnv, r.BaseEnv
		case *execbackend.InstanceRunner:
			return sandboxEnv, r.BaseEnv
		default:
			t.Fatalf("runner shape %T carries no base environment", runner)
			return sandboxEnv, nil
		}
	}
}

// #1810 review F5: the cockpit log tee REBUILT the adapter from the worker's
// execution runner and assigned over the composed one, discarding the whole
// read-only seat wrapper - no Landlock, no isolated state dir -
// and with produce grants already applied the rebuilt job would even receive a
// WRITE grant on the operator's live credential dir.
//
// This commit deletes the cockpit tee, so the test no longer has a cockpit call
// site. The PROPERTY survives and is retargeted rather than dropped: transcript
// output is attached with appendDeliveryAdapterOutput, which must ADD a writer to
// the composed adapter. If that ever rebuilds instead, a seat loses its sandbox
// the same way, and this test fails the same way.
func TestTranscriptOutputKeepsTheReadOnlySeatSandbox(t *testing.T) {
	home := t.TempDir()
	readOnlySeatOmpBrokerEnv(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	agent := runtime.Agent{Runtime: runtime.OmpRuntime, ReadOnlySeat: true}
	seatAdapter, _, err := wrapReadOnlySandboxAdapter(home, agent, checkout, "",
		runtime.OmpAdapter{Runner: subprocess.CuratedGroupRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	seat, ok := seatAdapter.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter %T, want readOnlyRuntimeAdapter", seatAdapter)
	}
	t.Cleanup(func() { _ = seat.cleanup() })

	logFile, err := openRetainedTranscriptLog(home, "seat-transcript-job", "review", runtime.OmpRuntime, 0, nil)
	if err != nil {
		t.Fatalf("openRetainedTranscriptLog: %v", err)
	}
	if logFile == nil {
		t.Fatal("expected a retained transcript log")
	}
	defer logFile.Close()

	teed, err := appendDeliveryAdapterOutput(seatAdapter, logFile)
	if err != nil {
		t.Fatalf("appendDeliveryAdapterOutput: %v", err)
	}
	teedSeat, ok := teed.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("attaching transcript output discarded the read-only seat wrapper: %T", teed)
	}
	inner, ok := teedSeat.Adapter.(runtime.OmpAdapter)
	if !ok {
		t.Fatalf("teed inner adapter %T, want runtime.OmpAdapter", teedSeat.Adapter)
	}
	sandboxEnv, _ := seatRunnerEnvironments(t, inner.Runner)
	if !containsEnvPrefix(sandboxEnv, "PI_CODING_AGENT_DIR=") {
		t.Fatalf("attaching transcript output dropped the isolated runtime state dir: %v", redactEnvNames(sandboxEnv))
	}
}
