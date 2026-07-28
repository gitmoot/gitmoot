package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestContractDocsEnumsMatchWorkflow(t *testing.T) {
	skill := filepath.Join("..", "..", "skills", "gitmoot", "SKILL.md")
	assertDocEnumNear(t, skill, "gitmoot job close", workflow.ResultDecisions)
	assertDocEnumNear(t, skill, "locks, commits, pushes", workflow.DelegationActions)
	assertDocEnumNear(t, skill, "gitmoot chat task", workflow.DelegationActions)

	contract := filepath.Join("..", "..", "skills", "gitmoot", "references", "RESULT_CONTRACT.md")
	assertDocEnumNear(t, contract, `"decision"`, workflow.ResultDecisions)
	assertDocEnumNear(t, contract, "failure_policy", workflow.DelegationFailurePolicies)
	assertDocEnumNear(t, contract, "synthesis_rule", workflow.DelegationSynthesisRules)
}

func assertDocEnumNear(t *testing.T, path, marker string, values []string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(contents), "\n")
	for i, line := range lines {
		if !strings.Contains(line, marker) {
			continue
		}
		end := min(i+3, len(lines))
		block := strings.Join(lines[i:end], " ")
		for _, value := range values {
			if !containsDocEnumToken(block, value) {
				t.Fatalf("%s enum near %q omits canonical value %q", path, marker, value)
			}
		}
		return
	}
	t.Fatalf("%s has no enum context containing %q", path, marker)
}

func TestContainsDocEnumTokenRejectsSubstring(t *testing.T) {
	if containsDocEnumToken("gitmoot chat task", "ask") {
		t.Fatal("ask must not match within task")
	}
	if !containsDocEnumToken("--action ask|review|implement", "ask") {
		t.Fatal("standalone ask was not matched")
	}
}

func containsDocEnumToken(text, value string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(value) + `\b`).MatchString(text)
}

func TestDocumentedGateBuildRunsInLinkedWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linked-worktree fixture uses Unix paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	agentsPath := filepath.Join("..", "..", "AGENTS.md")
	contents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read %s: %v", agentsPath, err)
	}
	const buildCommand = "go build -buildvcs=false ./..."
	const testCommand = "go test -timeout 30m ./..."
	if !strings.Contains(string(contents), buildCommand) {
		t.Fatalf("%s does not document %q", agentsPath, buildCommand)
	}
	if !strings.Contains(string(contents), testCommand) {
		t.Fatalf("%s does not document %q", agentsPath, testCommand)
	}

	// Go's VCS scanner ignores a linked worktree's .git file. A bogus ancestor
	// .git directory makes that failure deterministic on every host: an
	// unstamped build searches upward, chooses the ancestor, and runs git there.
	fixtureRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(fixtureRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainCheckout := filepath.Join(fixtureRoot, "main")
	if err := os.Mkdir(mainCheckout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainCheckout, "go.mod"), []byte("module example.com/worktreegate\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainCheckout, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainCheckout, "init")
	runGit(t, mainCheckout, "config", "user.email", "gate@example.invalid")
	runGit(t, mainCheckout, "config", "user.name", "Gate Test")
	runGit(t, mainCheckout, "add", "go.mod", "main.go")
	runGit(t, mainCheckout, "commit", "-m", "fixture")
	linkedCheckout := filepath.Join(fixtureRoot, "linked")
	runGit(t, mainCheckout, "worktree", "add", "--detach", linkedCheckout, "HEAD")

	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	commandEnv := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "GOCACHE=") || strings.HasPrefix(value, "GOENV=") ||
			strings.HasPrefix(value, "GOFLAGS=") || strings.HasPrefix(value, "GOTOOLCHAIN=") {
			continue
		}
		commandEnv = append(commandEnv, value)
	}
	commandEnv = append(commandEnv,
		"GOCACHE="+filepath.Join(fixtureRoot, "go-cache"),
		"GOENV=off",
		"GOFLAGS=",
		"GOTOOLCHAIN=local",
	)

	unstamped := exec.Command(goTool, "build", "./...")
	unstamped.Dir = linkedCheckout
	unstamped.Env = commandEnv
	output, err := unstamped.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "error obtaining VCS status") {
		t.Fatalf("fixture did not reproduce linked-worktree VCS failure: err=%v\n%s", err, output)
	}

	documented := exec.Command(goTool, "build", "-buildvcs=false", "./...")
	documented.Dir = linkedCheckout
	documented.Env = commandEnv
	if output, err := documented.CombinedOutput(); err != nil {
		t.Fatalf("%s failed in linked worktree: %v\n%s", buildCommand, err, output)
	}
}
