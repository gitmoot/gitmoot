package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const (
	KimiLiveCheckPrompt  = "Gitmoot Kimi live check. Return OK only."
	KimiAuthSetupMessage = "Kimi Code background jobs need a logged-in Kimi CLI. Run: kimi login, then restart the Gitmoot daemon so it inherits the session."

	// kimiMaxArgvPromptBytes is the conservative per-argument size ceiling for
	// the kimi prompt. Linux caps any SINGLE execve argument at MAX_ARG_STRLEN
	// (128 KiB = 32 * 4 KiB pages); a prompt at or above that fails fork/exec with
	// E2BIG ("argument list too long"), which killed live synth-judge calls (#723:
	// context + question + rubric + two embedded agent answers overflowed one arg).
	// Kimi Code exposes NO stdin or prompt-file channel — `-p, --prompt <prompt>`
	// is the only prompt input, and `-p -` is taken literally (the model receives a
	// bare "-", NOT stdin — probed against kimi-code 0.19.x). So an oversize prompt
	// is delivered by staging it to a temp file and pointing the agent's file-read
	// tool at it (see kimiPromptDelivery). We trip at 100 KiB, well below the hard
	// 128 KiB limit, to leave headroom for UTF-8/kernel accounting.
	kimiMaxArgvPromptBytes = 100 * 1024
)

// kimiPromptDelivery returns the string to pass as kimi's `-p` value, any extra
// argv flags the caller MUST append BEFORE `-p`, and a cleanup func the caller
// MUST defer. For a normal prompt (below kimiMaxArgvPromptBytes) it returns the
// prompt VERBATIM — byte-identical to the historical argv path — no extra args,
// and a no-op cleanup. For an oversize prompt it writes the prompt to a file in a
// dedicated temp DIRECTORY and returns a short wrapper instruction naming that
// file, keeping the argv small enough to clear MAX_ARG_STRLEN (see
// kimiMaxArgvPromptBytes).
//
// CRITICAL (#723 review): kimi-code's file-read tool is scoped to the session's
// workspace directories, so a file in the system temp dir is NOT readable by
// default — the wrapper would name a path the agent cannot open and the real
// prompt would be silently lost. We therefore grant the staged directory as an
// additional workspace via `--add-dir <dir>` (a kimi-code 0.19.x flag: "Add an
// additional workspace directory for this session"), which is returned in
// extraArgs. Staging into a dedicated temp dir (not the job worktree) keeps the
// file out of any implement job's `git add`, and RemoveAll on cleanup leaves
// nothing behind.
func kimiPromptDelivery(prompt string) (promptArg string, extraArgs []string, cleanup func(), err error) {
	noop := func() {}
	if len(prompt) < kimiMaxArgvPromptBytes {
		return prompt, nil, noop, nil
	}
	dir, err := os.MkdirTemp("", "gitmoot-kimi-prompt-*")
	if err != nil {
		return "", nil, noop, fmt.Errorf("stage oversize kimi prompt: %w", err)
	}
	remove := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		remove()
		return "", nil, noop, fmt.Errorf("write oversize kimi prompt: %w", err)
	}
	return kimiOversizePromptWrapper(path), []string{"--add-dir", dir}, remove, nil
}

// kimiOversizePromptWrapper is the small argv-safe instruction that stands in for
// an oversize prompt: it names the temp file holding the real prompt and tells the
// agent to read the whole file and follow it exactly, preserving the original
// prompt's output contract (the instructions live inside the file).
func kimiOversizePromptWrapper(path string) string {
	return fmt.Sprintf("Your complete task, all of its context, and the exact output format you must produce "+
		"are written in the file %s. Read that entire file first using your file-read tool, then carry out "+
		"the task exactly as written there and respond exactly as it instructs. The file is your prompt — do "+
		"not ask for its contents and do not treat the path itself as the task.", path)
}

// KimiAdapter delivers Gitmoot jobs to the local Kimi Code CLI.
// Kimi Code supports non-interactive prompt mode (`-p`), structured stream-json
// output, and session resume (`-S <session_id>`). The adapter extracts the
// assistant response and the session id from the JSONL stream. Keep this runtime
// print-less: kimi-code does not support `--print`.
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
	promptArg, extraArgs, cleanup, err := kimiPromptDelivery(request.Prompt)
	if err != nil {
		return StartResult{}, err
	}
	defer cleanup()
	args = append(args, extraArgs...)
	args = append(args, "-p", promptArg, "--output-format", "stream-json")
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
	if agent.RuntimeRef != "" && !isKimiSessionID(agent.RuntimeRef) && !IsFreshRef(agent.RuntimeRef) {
		return fmt.Errorf("kimi runtime reference %q must be a Kimi session id, fresh:<suffix>, or empty", agent.RuntimeRef)
	}
	return nil
}

func (a KimiAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	args := kimiPermissionArgs(agent)
	model := EffectiveModel(agent, job)
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
	promptArg, extraArgs, cleanup, err := kimiPromptDelivery(job.Prompt)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	args = append(args, extraArgs...)
	args = append(args, "-p", promptArg, "--output-format", "stream-json")
	result, err := runAgentCommand(ctx, a.runner(), a.Dir, job.AgentEnv, "kimi", args...)
	// The fresh per-job kimi session never reports its id, so SessionID stays
	// empty; the exit/stderr diagnostics still apply.
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, "")}, kimiCommandError(result, err)
	}
	content, _, usage, parseErr := parseKimiStreamJSON(result.Stdout)
	if parseErr != nil {
		return Result{Raw: result.Stdout, SessionDiag: newSessionDiag(result, nil, "")}, fmt.Errorf("parse kimi stream-json output: %w", parseErr)
	}
	return Result{Raw: content, Summary: strings.TrimSpace(content), InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, SessionDiag: newSessionDiag(result, nil, "")}, nil
}

func (a KimiAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: KimiLiveCheckPrompt})
	return err
}

func (a KimiAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask", "produce"}, nil
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

// KimiCLIAdapter delivers jobs to the opt-in legacy Kimi CLI runtime. This
// runtime is intentionally separate from `kimi`: it uses the older `--print`
// command shape without probing or changing the default Kimi Code path.
type KimiCLIAdapter struct {
	Runner        subprocess.Runner
	Dir           string
	NewRuntimeRef func() (string, error)
}

func (a KimiCLIAdapter) Name() string { return KimiCLIRuntime }

func (a KimiCLIAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	runtimeRef, err := a.newRuntimeRef()
	if err != nil {
		return StartResult{}, err
	}
	promptArg, extraArgs, cleanup, err := kimiPromptDelivery(request.Prompt)
	if err != nil {
		return StartResult{}, err
	}
	defer cleanup()
	args := kimiCLIPromptArgs(request.Agent, request.Agent.Model, promptArg, extraArgs)
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

func (a KimiCLIAdapter) Validate(_ context.Context, agent Agent) error {
	if err := validateRuntime(agent, a.Name()); err != nil {
		return err
	}
	if agent.RuntimeRef != "" && !isKimiSessionID(agent.RuntimeRef) && !IsFreshRef(agent.RuntimeRef) {
		return fmt.Errorf("kimi runtime reference %q must be a Kimi session id, fresh:<suffix>, or empty", agent.RuntimeRef)
	}
	return nil
}

func (a KimiCLIAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	_ = agent.RuntimeRef
	promptArg, extraArgs, cleanup, err := kimiPromptDelivery(job.Prompt)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	result, err := runAgentCommand(ctx, a.runner(), a.Dir, job.AgentEnv, "kimi", kimiCLIPromptArgs(agent, EffectiveModel(agent, job), promptArg, extraArgs)...)
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr, SessionDiag: newSessionDiag(result, err, "")}, kimiCommandError(result, err)
	}
	content, _, usage, parseErr := parseKimiStreamJSON(result.Stdout)
	if parseErr != nil {
		return Result{Raw: result.Stdout, SessionDiag: newSessionDiag(result, nil, "")}, fmt.Errorf("parse kimi stream-json output: %w", parseErr)
	}
	return Result{Raw: content, Summary: strings.TrimSpace(content), InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, SessionDiag: newSessionDiag(result, nil, "")}, nil
}

func (a KimiCLIAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: KimiLiveCheckPrompt})
	return err
}

func (a KimiCLIAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a KimiCLIAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.GroupRunner{}
}

func (a KimiCLIAdapter) newRuntimeRef() (string, error) {
	if a.NewRuntimeRef != nil {
		return a.NewRuntimeRef()
	}
	return newUUID()
}

// kimiCLIPromptArgs builds the legacy kimi-cli `--print -p` argument vector.
// promptArg and extraArgs are the values already produced by kimiPromptDelivery
// (the verbatim prompt with no extra args for normal sizes, or the argv-safe
// temp-file wrapper plus the `--add-dir <dir>` workspace grant for oversize
// prompts), so this runtime shares the #723 MAX_ARG_STRLEN protection AND the
// workspace grant that makes the staged prompt file readable.
func kimiCLIPromptArgs(agent Agent, model string, promptArg string, extraArgs []string) []string {
	args := kimiPermissionArgs(agent)
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, extraArgs...)
	return append(args, "--print", "-p", promptArg, "--output-format", "stream-json")
}

func kimiPermissionArgs(agent Agent) []string {
	// Kimi's prompt mode already runs non-interactively. The --yolo and --auto
	// flags cannot be combined with -p. Produce paths are still passed as
	// cooperative workspace hints; Gitmoot's Landlock wrapper is the enforcement
	// boundary and the flags only let Kimi's own file layer see the same roots.
	var args []string
	for _, path := range agent.WritablePaths {
		if path = strings.TrimSpace(path); path != "" {
			args = append(args, "--add-dir", path)
		}
	}
	for _, path := range agent.ReadablePaths {
		if path = strings.TrimSpace(path); path != "" {
			// Kimi's workspace guard needs the same visibility hint; the
			// surrounding Landlock policy prevents writes to readable roots.
			args = append(args, "--add-dir", path)
		}
	}
	return args
}

// kimiUsage holds the best-effort token counts extracted from a Kimi stream-json
// usage/result event. Counts default to 0 when the stream omits usage.
type kimiUsage = StreamUsage

// parseKimiStreamJSON reads Kimi's --output-format stream-json JSONL output and
// returns the concatenated assistant content, the session id from the resume hint
// meta event (if present), and the best-effort token usage from the last event
// that carried a usage object (if any). Usage capture is best-effort: when no
// event reports usage the returned kimiUsage is zero-valued and the job
// contributes 0 to the per-root token budget.
//
// UPSTREAM LIMITATION (#659): kimi-code 0.19.2 — the CLI Gitmoot targets today —
// emits NO usage event anywhere in its stream. A prompt-mode run yields only an
// `assistant` event and a `meta` session.resume_hint event, so every kimi job
// reports 0/0 by upstream limitation, not a parser bug (confirmed against
// `kimi --help`: 0.19.2 has no --verbose/--stats/--include-usage flag that would
// enable usage in the stream). The usage extraction below is retained for
// older/newer Kimi CLIs that DO emit a usage/result event.
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
		event, err := ExtractKimiStreamEvent(line)
		if err != nil {
			continue
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
		switch event.Role {
		case "assistant":
			if content := event.ContentText; content != "" {
				contentParts = append(contentParts, content)
			}
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

func isKimiRuntime(runtimeName string) bool {
	switch runtimeName {
	case KimiRuntime, KimiCLIRuntime:
		return true
	default:
		return false
	}
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
