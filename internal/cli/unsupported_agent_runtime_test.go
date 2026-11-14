package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestAgentListAndDoctorNameAgentsOnRemovedRuntimes covers the #2587 data
// path: agent rows and [agents.*] types still on codex, claude or kimi keep
// loading, and both `agent list` and `doctor` name each one as unsupported with
// the omp-required message, while omp and shell agents are not reported.
func TestAgentListAndDoctorNameAgentsOnRemovedRuntimes(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	for _, agent := range []db.Agent{
		{Name: "old-codex", Runtime: "codex", RuntimeRef: "fresh", Role: "reviewer"},
		{Name: "old-claude", Runtime: "claude", RuntimeRef: "fresh", Role: "reviewer"},
		{Name: "current-omp", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh", Role: "reviewer"},
		{Name: "tests-shell", Runtime: runtime.ShellRuntime, RuntimeRef: "true", Role: "reviewer"},
	} {
		if err := store.UpsertAgent(context.Background(), agent); err != nil {
			t.Fatalf("UpsertAgent(%s): %v", agent.Name, err)
		}
	}
	store.Close()
	paths := config.PathsForHome(home)
	configFile, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := configFile.WriteString("\n[agents.kimi-type]\nruntime = \"kimi\"\n"); err != nil {
		t.Fatalf("append agent type: %v", err)
	}
	configFile.Close()

	wantNamed := []string{
		`reviewer old-codex: removed job runtime: runtime "codex" is no longer supported; gitmoot requires omp`,
		`reviewer old-claude: removed job runtime: runtime "claude" is no longer supported; gitmoot requires omp`,
		`reviewer type kimi-type: removed job runtime: runtime "kimi" is no longer supported; gitmoot requires omp`,
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"reviewer", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent list code = %d, stderr = %s", code, stderr.String())
	}
	for _, name := range []string{"old-codex", "old-claude", "current-omp", "tests-shell"} {
		if !strings.Contains(stdout.String(), name) {
			t.Fatalf("agent list stdout lacks %s (rows must stay listed):\n%s", name, stdout.String())
		}
	}
	for _, want := range wantNamed {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("agent list stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	for _, supported := range []string{"current-omp", "tests-shell"} {
		if strings.Contains(stderr.String(), supported) {
			t.Fatalf("agent list reported supported agent %s as unsupported:\n%s", supported, stderr.String())
		}
	}

	check, ok := unsupportedAgentRuntimeDoctorCheck(paths)
	if !ok {
		t.Fatal("unsupportedAgentRuntimeDoctorCheck ok=false for an initialized home")
	}
	if check.OK || check.Name != "reviewer runtimes" || !strings.HasPrefix(check.Detail, "3 unsupported: ") {
		t.Fatalf("doctor check = %+v, want a failing 'agent runtimes' check naming 3 agents", check)
	}
	for _, want := range wantNamed {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("doctor detail lacks %q:\n%s", want, check.Detail)
		}
	}

	clean, ok := unsupportedAgentRuntimeDoctorCheck(config.PathsForHome(t.TempDir()))
	if ok {
		t.Fatalf("uninitialized home produced a check: %+v", clean)
	}
}
