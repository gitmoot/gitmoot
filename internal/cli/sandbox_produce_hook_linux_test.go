//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestClaudeProduceHookAutoReadLandlockE2E(t *testing.T) {
	abi, err := sandbox.ABI()
	if err != nil || abi < sandbox.MinimumABI {
		t.Skipf("Landlock ABI v%d unavailable (need v%d): %v", abi, sandbox.MinimumABI, err)
	}
	base, err := os.MkdirTemp(".", ".gitmoot-claude-hook-sandbox-*")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)

	operatorHome := filepath.Join(base, "operator")
	configDir := filepath.Join(operatorHome, ".claude")
	hookDir := filepath.Join(base, "hooks")
	siblingDir := filepath.Join(base, "not-included")
	inputDir := filepath.Join(base, "input")
	outputDir := filepath.Join(base, "output")
	workdir := filepath.Join(base, "work")
	binDir := filepath.Join(base, "bin")
	for _, dir := range []string{configDir, hookDir, siblingDir, inputDir, outputDir, workdir, binDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", operatorHome)
	t.Setenv(runtime.ClaudeConfigDirEnv, "")
	hookPath := filepath.Join(hookDir, "prompt-hook.sh")
	siblingPath := filepath.Join(siblingDir, "private.txt")
	outputPath := filepath.Join(outputDir, "result.txt")
	if err := os.WriteFile(hookPath, []byte("hook-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingPath, []byte("private-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaudeProduceSettings(t, configDir, fmt.Sprintf(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":%q}]}]}}`, hookPath))
	userState := filepath.Join(operatorHome, ".claude.json")
	if err := os.WriteFile(userState, []byte(`{"operator":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(base, "config-home")
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createProduceGrantPipeline(t, store, inputDir, outputDir)
	agent := runtime.Agent{Name: "producer", Runtime: runtime.ClaudeRuntime}
	if err := applyProduceRuntimeGrants(context.Background(), store, home, db.Job{ID: "landlock-hook", Type: "produce"}, workflow.JobPayload{
		PipelineName: "p", WritablePaths: []string{outputDir}, ReadablePaths: []string{inputDir},
	}, &agent); err != nil {
		t.Fatal(err)
	}
	if !containsString(agent.ReadablePaths, hookDir) {
		t.Fatalf("hook directory not auto-included: %v", agent.ReadablePaths)
	}

	gitmoot := filepath.Join(binDir, "gitmoot")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", gitmoot, "./cmd/gitmoot")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gitmoot test binary: %v\n%s", err, output)
	}
	reads, readFiles, writes, env, err := produceRuntimeSandboxGrants(agent.Runtime, agent.ReadablePaths, agent.ReadableFiles, agent.WritablePaths)
	if err != nil {
		t.Fatal(err)
	}
	runner := subprocess.WrappingRunner{
		Inner:         subprocess.GroupRunner{},
		Executable:    gitmoot,
		ReadablePaths: reads,
		ReadableFiles: readFiles,
		WritablePaths: writes,
		Env:           env,
	}
	script := `set -eu
cat "$1" >/dev/null
cat "$2" >/dev/null
if { printf denied > "$1"; } 2>/dev/null; then exit 41; fi
if cat "$3" >/dev/null 2>&1; then exit 42; fi
printf ok > "$4"
`
	result, err := runner.Run(context.Background(), workdir, "/bin/sh", "-c", script, "gitmoot-test", hookPath, userState, siblingPath, outputPath)
	if err != nil {
		t.Fatalf("Landlock hook read test failed: %v\nstdout=%s\nstderr=%s", err, result.Stdout, result.Stderr)
	}
	if data, err := os.ReadFile(outputPath); err != nil || string(data) != "ok" {
		t.Fatalf("output = %q, err=%v", data, err)
	}
	if data, err := os.ReadFile(hookPath); err != nil || string(data) != "hook-data" {
		t.Fatalf("hook was modified: %q, err=%v", data, err)
	}
}
