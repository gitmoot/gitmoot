package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const (
	KimiLiveCheckPrompt  = "Gitmoot Kimi live check. Return OK only."
	KimiAuthSetupMessage = "Kimi Code background jobs need a logged-in Kimi CLI. Run: kimi login, then restart the Gitmoot daemon so it inherits the session."
)

// KimiAdapter delivers Gitmoot jobs to the local Kimi Code CLI.
// Kimi supports non-interactive prompt mode (`-p`), structured stream-json output,
// and session resume (`-S <session_id>`). The adapter extracts the assistant
// response and the session id from the JSONL stream.
type KimiAdapter struct {
	Runner        subprocess.Runner
	Dir           string
	NewRuntimeRef func() (string, error)
}

func (a KimiAdapter) Name() string { return KimiRuntime }

func (a KimiAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	// RuntimeRef is only used as a fallback if the stream does not report a session id.
	runtimeRef, err := a.newRuntimeRef()
	if err != nil {
		return StartResult{}, err
	}
	args := kimiPermissionArgs(request.Agent)
	if request.Agent.Model != "" {
		args = append(args, "--model", request.Agent.Model)
	}
	args = append(args, "-p", request.Prompt, "--output-format", "stream-json")
	result, err := a.runner().Run(ctx, a.Dir, "kimi", args...)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, kimiCommandError(result, err)
	}
	content, sessionID, _, parseErr := parseKimiStreamJSON(result.Stdout)
	if parseErr != nil {
		return StartResult{Raw: result.Stdout}, fmt.Errorf("parse kimi stream-json output: %w", parseErr)
	}
	if sessionID == "" {
		sessionID = runtimeRef
	}
	return StartResult{RuntimeRef: sessionID, Raw: content}, nil
}

func (a KimiAdapter) Validate(_ context.Context, agent Agent) error {
	if err := validateRuntime(agent, a.Name()); err != nil {
		return err
	}
	if agent.RuntimeRef != "" && !isKimiSessionID(agent.RuntimeRef) {
		return fmt.Errorf("kimi runtime reference %q must be a Kimi session id or empty", agent.RuntimeRef)
	}
	return nil
}

func (a KimiAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	args := kimiPermissionArgs(agent)
	model := effectiveModel(agent, job)
	if model != "" {
		args = append(args, "--model", model)
	}
	// Kimi Code sessions are DIRECTORY-SCOPED, and Gitmoot runs each job in its own
	// worktree, so resuming a session created elsewhere fails ("Session ... was created
	// under a different directory"). Gitmoot jobs are independent (the full context is in
	// job.Prompt), so start a FRESH session per job (no -S). This works in any worktree
	// and keeps Kimi a native runtime seat. agent.RuntimeRef stays validated but is not
	// used to resume across directories.
	_ = agent.RuntimeRef
	args = append(args, "-p", job.Prompt, "--output-format", "stream-json")
	result, err := a.runner().Run(ctx, a.Dir, "kimi", args...)
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr}, kimiCommandError(result, err)
	}
	content, _, usage, parseErr := parseKimiStreamJSON(result.Stdout)
	if parseErr != nil {
		return Result{Raw: result.Stdout}, fmt.Errorf("parse kimi stream-json output: %w", parseErr)
	}
	return Result{Raw: content, Summary: strings.TrimSpace(content), InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}, nil
}

func (a KimiAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: KimiLiveCheckPrompt})
	return err
}

func (a KimiAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a KimiAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.GroupRunner{}
}

func (a KimiAdapter) newRuntimeRef() (string, error) {
	if a.NewRuntimeRef != nil {
		return a.NewRuntimeRef()
	}
	return newUUID()
}

func kimiPermissionArgs(agent Agent) []string {
	// Kimi's `-p` prompt mode already runs non-interactively and auto-approves
	// tool calls. The --yolo and --auto flags cannot be combined with -p, so we
	// pass no extra permission flags. Read-only enforcement is handled by the
	// agent's capabilities and Gitmoot's dispatch/lock logic.
	_ = agent
	return nil
}

type kimiStreamEvent struct {
	Role      string `json:"role"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	SessionID string `json:"session_id"`
	// Usage carries best-effort token counts when Kimi's stream emits a usage or
	// result event (#338 Part B). The field set mirrors the common LLM-CLI shape
	// (input_tokens/output_tokens). It is nil/zero on events that carry no usage,
	// so a runtime that never reports usage simply contributes 0 to the budget.
	Usage *kimiUsage `json:"usage"`
}

// kimiUsage holds the best-effort token counts extracted from a Kimi stream-json
// usage/result event. Counts default to 0 when the stream omits usage.
type kimiUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseKimiStreamJSON reads Kimi's --output-format stream-json JSONL output and
// returns the concatenated assistant content, the session id from the resume hint
// meta event (if present), and the best-effort token usage from the last event
// that carried a usage object (if any). Usage capture is best-effort: when no
// event reports usage the returned kimiUsage is zero-valued and the job
// contributes 0 to the per-root token budget.
func parseKimiStreamJSON(output string) (string, string, kimiUsage, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var contentParts []string
	var sessionID string
	var usage kimiUsage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event kimiStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
		switch event.Role {
		case "assistant":
			contentParts = append(contentParts, event.Content)
		case "meta":
			if event.Type == "session.resume_hint" && event.SessionID != "" {
				sessionID = event.SessionID
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", kimiUsage{}, err
	}
	return strings.Join(contentParts, ""), sessionID, usage, nil
}

func isKimiSessionID(ref string) bool {
	return strings.HasPrefix(ref, "session_")
}

func kimiCommandError(result subprocess.Result, err error) error {
	base := commandError(result, err)
	if !isKimiAuthFailure(result) {
		return base
	}
	return fmt.Errorf("Kimi Code authentication required. %s: %w", KimiAuthSetupMessage, base)
}

func isKimiAuthFailure(result subprocess.Result) bool {
	text := strings.ToLower(strings.Join([]string{result.Stdout, result.Stderr}, "\n"))
	return strings.Contains(text, "login") ||
		strings.Contains(text, "authenticate") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "authentication")
}
