package runtime

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// captureKimiRunner records the argv of each invocation and, so an oversize-prompt
// temp file can be inspected before its deferred cleanup removes it, reads any
// staged prompt file WHILE it still exists (during Run, before the adapter returns).
type captureKimiRunner struct {
	result     subprocess.Result
	lastArgs   []string
	promptArg  string
	stagedPath string
	stagedBody string
	stagedRead bool
}

var kimiTempPromptPathRE = regexp.MustCompile(`\S*gitmoot-kimi-prompt-\S+\.txt`)

func (r *captureKimiRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.lastArgs = append([]string{command}, args...)
	// Extract the value passed to -p.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-p" {
			r.promptArg = args[i+1]
			break
		}
	}
	if m := kimiTempPromptPathRE.FindString(r.promptArg); m != "" {
		r.stagedPath = m
		if body, err := os.ReadFile(m); err == nil {
			r.stagedBody = string(body)
			r.stagedRead = true
		}
	}
	res := r.result
	res.Command = command
	res.Args = args
	return res, nil
}

func (r *captureKimiRunner) LookPath(file string) (string, error) { return "/usr/bin/" + file, nil }

const kimiStreamOK = `{"role":"assistant","content":"OK"}` + "\n"

func kimiTestAgent() Agent {
	return Agent{
		Name:           "reviewer",
		Role:           "reviewer",
		Runtime:        KimiRuntime,
		RuntimeRef:     "session_550e8400-e29b-41d4-a716-446655440001",
		RepoScope:      "jerryfane/gitmoot",
		AutonomyPolicy: AutonomyPolicyReadOnly,
	}
}

// TestKimiPromptDeliveryShortPromptIsVerbatim asserts a normal-size prompt is
// passed verbatim as the -p value (byte-identical to the historical argv path)
// and stages no temp file.
func TestKimiPromptDeliveryShortPromptIsVerbatim(t *testing.T) {
	prompt := "review this small change"
	arg, extraArgs, cleanup, err := kimiPromptDelivery(prompt)
	if err != nil {
		t.Fatalf("kimiPromptDelivery: %v", err)
	}
	defer cleanup()
	if arg != prompt {
		t.Fatalf("short prompt not passed verbatim: got %q want %q", arg, prompt)
	}
	if len(extraArgs) != 0 {
		t.Fatalf("short prompt should add no argv flags, got %v", extraArgs)
	}
	if kimiTempPromptPathRE.MatchString(arg) {
		t.Fatalf("short prompt unexpectedly staged a temp file: %q", arg)
	}
}

// TestKimiPromptDeliveryBoundaryJustBelowThreshold: a prompt one byte under the
// threshold must still go through argv unchanged.
func TestKimiPromptDeliveryBoundaryJustBelowThreshold(t *testing.T) {
	prompt := strings.Repeat("x", kimiMaxArgvPromptBytes-1)
	arg, extraArgs, cleanup, err := kimiPromptDelivery(prompt)
	if err != nil {
		t.Fatalf("kimiPromptDelivery: %v", err)
	}
	defer cleanup()
	if arg != prompt {
		t.Fatalf("prompt at threshold-1 should be verbatim argv; got len=%d staged=%v", len(arg), kimiTempPromptPathRE.MatchString(arg))
	}
	if len(extraArgs) != 0 {
		t.Fatalf("prompt at threshold-1 should add no argv flags, got %v", extraArgs)
	}
}

// TestKimiPromptDeliveryBoundaryAtThreshold: a prompt exactly at the threshold
// must be staged to a temp file whose content is the original prompt intact, and
// the returned arg is the argv-safe wrapper (much shorter than the prompt).
func TestKimiPromptDeliveryBoundaryAtThreshold(t *testing.T) {
	prompt := strings.Repeat("y", kimiMaxArgvPromptBytes)
	arg, extraArgs, cleanup, err := kimiPromptDelivery(prompt)
	if err != nil {
		t.Fatalf("kimiPromptDelivery: %v", err)
	}
	defer cleanup()
	path := kimiTempPromptPathRE.FindString(arg)
	if path == "" {
		t.Fatalf("oversize prompt was not staged to a temp file; arg=%.80q...", arg)
	}
	if len(arg) >= kimiMaxArgvPromptBytes {
		t.Fatalf("wrapper arg is not argv-safe: len=%d (>= threshold %d)", len(arg), kimiMaxArgvPromptBytes)
	}
	// The staged directory MUST be granted to kimi via --add-dir, else kimi's
	// workspace-scoped file-read tool cannot open the staged prompt (#723 review).
	grant := kimiAddDirValue(extraArgs)
	if grant == "" {
		t.Fatalf("oversize prompt did not grant --add-dir; extraArgs=%v", extraArgs)
	}
	if filepath.Dir(path) != grant {
		t.Fatalf("--add-dir %q does not contain staged prompt file %q", grant, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged prompt file: %v", err)
	}
	if string(body) != prompt {
		t.Fatalf("staged prompt content mismatch: got len=%d want len=%d", len(body), len(prompt))
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove temp file %s (err=%v)", path, err)
	}
	if _, err := os.Stat(grant); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove staged dir %s (err=%v)", grant, err)
	}
}

// kimiAddDirValue returns the value passed to the last --add-dir flag in args, or
// "" if none is present.
func kimiAddDirValue(args []string) string {
	dir := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--add-dir" {
			dir = args[i+1]
		}
	}
	return dir
}

// TestKimiDeliverShortPromptUnchanged is the no-LLM E2E: a normal prompt drives
// KimiAdapter.Deliver end to end through a fake runner and the -p value is the
// verbatim prompt (no temp file), exactly as before #723.
func TestKimiDeliverShortPromptUnchanged(t *testing.T) {
	runner := &captureKimiRunner{result: subprocess.Result{Stdout: kimiStreamOK}}
	adapter := KimiAdapter{Runner: runner}
	prompt := "review this PR"
	if _, err := adapter.Deliver(context.Background(), kimiTestAgent(), Job{Prompt: prompt}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if runner.promptArg != prompt {
		t.Fatalf("-p value = %q, want verbatim %q", runner.promptArg, prompt)
	}
	if runner.stagedPath != "" {
		t.Fatalf("short prompt unexpectedly staged file %s", runner.stagedPath)
	}
	// Argv shape is otherwise unchanged.
	want := []string{"kimi", "-p", prompt, "--output-format", "stream-json"}
	if strings.Join(runner.lastArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %v, want %v", runner.lastArgs, want)
	}
}

// TestKimiDeliverLongPromptUsesTempFile is the no-LLM E2E for the fix: an oversize
// prompt drives KimiAdapter.Deliver, the -p value is the argv-safe wrapper naming
// a temp file, and that file holds the original prompt intact. The temp file is
// read during Run (before the deferred cleanup fires).
func TestKimiDeliverLongPromptUsesTempFile(t *testing.T) {
	runner := &captureKimiRunner{result: subprocess.Result{Stdout: kimiStreamOK}}
	adapter := KimiAdapter{Runner: runner}
	prompt := strings.Repeat("Z", kimiMaxArgvPromptBytes+4096)
	if _, err := adapter.Deliver(context.Background(), kimiTestAgent(), Job{Prompt: prompt}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if runner.stagedPath == "" {
		t.Fatalf("oversize prompt was not staged to a temp file; -p=%.80q...", runner.promptArg)
	}
	if !runner.stagedRead {
		t.Fatalf("temp file %s did not exist during Run", runner.stagedPath)
	}
	if runner.stagedBody != prompt {
		t.Fatalf("staged prompt content mismatch: got len=%d want len=%d", len(runner.stagedBody), len(prompt))
	}
	if len(runner.promptArg) >= kimiMaxArgvPromptBytes {
		t.Fatalf("wrapper -p arg is not argv-safe: len=%d", len(runner.promptArg))
	}
	// The staged dir must be granted to kimi via --add-dir so its workspace-scoped
	// file-read tool can open the staged prompt (#723 review).
	grant := kimiAddDirValue(runner.lastArgs)
	if grant == "" {
		t.Fatalf("Deliver did not grant --add-dir for oversize prompt; argv=%v", runner.lastArgs)
	}
	if filepath.Dir(runner.stagedPath) != grant {
		t.Fatalf("--add-dir %q does not contain staged prompt %q", grant, runner.stagedPath)
	}
	// The huge prompt must NOT appear in the argv anywhere.
	if strings.Contains(strings.Join(runner.lastArgs, ""), prompt) {
		t.Fatal("raw oversize prompt leaked into argv despite temp-file delivery")
	}
	// After Deliver returns, the deferred cleanup must have removed the temp file.
	if _, err := os.Stat(runner.stagedPath); !os.IsNotExist(err) {
		t.Fatalf("temp file %s was not cleaned up after Deliver (err=%v)", runner.stagedPath, err)
	}
}

// TestKimiStartLongPromptUsesTempFile confirms Start shares the protection.
func TestKimiStartLongPromptUsesTempFile(t *testing.T) {
	stream := `{"role":"meta","type":"session.resume_hint","session_id":"session_550e8400-e29b-41d4-a716-446655440000"}` + "\n"
	runner := &captureKimiRunner{result: subprocess.Result{Stdout: stream}}
	adapter := KimiAdapter{Runner: runner, Dir: "/repo"}
	agent := Agent{Name: "lead", Role: "implementer", Runtime: KimiRuntime, RepoScope: "jerryfane/gitmoot", AutonomyPolicy: AutonomyPolicyReadOnly}
	prompt := strings.Repeat("Q", kimiMaxArgvPromptBytes+10)
	if _, err := adapter.Start(context.Background(), StartRequest{Agent: agent, Prompt: prompt}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !runner.stagedRead || runner.stagedBody != prompt {
		t.Fatalf("Start did not stage the oversize prompt intact (read=%v)", runner.stagedRead)
	}
	if grant := kimiAddDirValue(runner.lastArgs); grant == "" || filepath.Dir(runner.stagedPath) != grant {
		t.Fatalf("Start did not grant --add-dir for staged prompt %q; argv=%v", runner.stagedPath, runner.lastArgs)
	}
	if _, err := os.Stat(runner.stagedPath); !os.IsNotExist(err) {
		t.Fatalf("Start left temp file %s behind", runner.stagedPath)
	}
}

// TestKimiCLILongPromptUsesTempFile confirms the legacy kimi-cli runtime shares
// the protection and still emits its --print flag.
func TestKimiCLILongPromptUsesTempFile(t *testing.T) {
	runner := &captureKimiRunner{result: subprocess.Result{Stdout: kimiStreamOK}}
	adapter := KimiCLIAdapter{Runner: runner}
	agent := kimiTestAgent()
	agent.Runtime = KimiCLIRuntime
	prompt := strings.Repeat("W", kimiMaxArgvPromptBytes+1)
	if _, err := adapter.Deliver(context.Background(), agent, Job{Prompt: prompt}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !runner.stagedRead || runner.stagedBody != prompt {
		t.Fatalf("legacy kimi-cli did not stage the oversize prompt intact (read=%v)", runner.stagedRead)
	}
	joined := strings.Join(runner.lastArgs, "\x00")
	if !strings.Contains(joined, "--print") {
		t.Fatalf("legacy kimi-cli dropped --print: %v", runner.lastArgs)
	}
	if grant := kimiAddDirValue(runner.lastArgs); grant == "" || filepath.Dir(runner.stagedPath) != grant {
		t.Fatalf("legacy kimi-cli did not grant --add-dir for staged prompt %q; argv=%v", runner.stagedPath, runner.lastArgs)
	}
	if strings.Contains(strings.Join(runner.lastArgs, ""), prompt) {
		t.Fatal("raw oversize prompt leaked into legacy kimi-cli argv")
	}
}
