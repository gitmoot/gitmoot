package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const (
	CodexRuntime  = "codex"
	ClaudeRuntime = "claude"
	ShellRuntime  = "shell"
	LastRef       = "last"

	healthPrompt = "Gitmoot health check. Reply OK only."

	AutonomyPolicyAuto             = "auto"
	AutonomyPolicyReadOnly         = "read-only"
	AutonomyPolicyWorkspaceWrite   = "workspace-write"
	AutonomyPolicyDangerFullAccess = "danger-full-access"
)

type Agent struct {
	Name           string
	Role           string
	Runtime        string
	RuntimeRef     string
	RepoScope      string
	TemplateID     string
	Capabilities   []string
	AutonomyPolicy string
	HealthStatus   string
}

type Job struct {
	ID          string
	AgentName   string
	Action      string
	Prompt      string
	Repository  string
	PullRequest int
}

type Result struct {
	Decision string
	Summary  string
	Raw      string
}

type StartRequest struct {
	Agent  Agent
	Prompt string
}

type StartResult struct {
	RuntimeRef string
	Raw        string
}

type Adapter interface {
	Name() string
	Start(ctx context.Context, request StartRequest) (StartResult, error)
	Validate(ctx context.Context, agent Agent) error
	Deliver(ctx context.Context, agent Agent, job Job) (Result, error)
	Health(ctx context.Context, agent Agent) error
	Capabilities(ctx context.Context) ([]string, error)
}

type Factory struct {
	Runner subprocess.Runner
}

func (f Factory) Adapter(name string) (Adapter, error) {
	switch name {
	case CodexRuntime:
		return CodexAdapter{Runner: f.Runner}, nil
	case ClaudeRuntime:
		return ClaudeAdapter{Runner: f.Runner}, nil
	case ShellRuntime:
		return ShellAdapter{Runner: f.Runner}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", name)
	}
}

func ValidateAgent(agent Agent) error {
	if err := validateAgentFields(agent, true); err != nil {
		return err
	}
	if agent.Runtime == ClaudeRuntime && agent.RuntimeRef != LastRef && !isUUID(agent.RuntimeRef) {
		return fmt.Errorf("claude runtime reference %q must be a UUID or last", agent.RuntimeRef)
	}
	return nil
}

func ValidateStartRequest(request StartRequest) error {
	adapter, err := (Factory{}).Adapter(request.Agent.Runtime)
	if err != nil {
		return err
	}
	return validateStartRequest(request.Agent, adapter.Name(), request.Prompt)
}

func validateStartRequest(agent Agent, runtimeName string, prompt string) error {
	if err := validateAgentFields(agent, false); err != nil {
		return err
	}
	if agent.Runtime != runtimeName {
		return fmt.Errorf("agent runtime %q does not match adapter %q", agent.Runtime, runtimeName)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("start prompt is required")
	}
	return nil
}

func validateAgentFields(agent Agent, requireRuntimeRef bool) error {
	if _, err := NormalizeAutonomyPolicy(agent.AutonomyPolicy); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(agent.Name) == "":
		return errors.New("agent name is required")
	case strings.ContainsAny(agent.Name, " \t\n/"):
		return fmt.Errorf("agent name %q cannot contain whitespace or slash", agent.Name)
	case strings.TrimSpace(agent.Role) == "":
		return errors.New("agent role is required")
	case strings.TrimSpace(agent.Runtime) == "":
		return errors.New("agent runtime is required")
	case requireRuntimeRef && strings.TrimSpace(agent.RuntimeRef) == "":
		return errors.New("agent runtime reference is required")
	case strings.TrimSpace(agent.RepoScope) != "" && !validRepoScope(agent.RepoScope):
		return fmt.Errorf("agent repo scope %q must be owner/repo", agent.RepoScope)
	}
	if _, err := (Factory{}).Adapter(agent.Runtime); err != nil {
		return err
	}
	return nil
}

func NormalizeAutonomyPolicy(policy string) (string, error) {
	switch strings.TrimSpace(policy) {
	case "", AutonomyPolicyAuto:
		return AutonomyPolicyAuto, nil
	case AutonomyPolicyReadOnly:
		return AutonomyPolicyReadOnly, nil
	case AutonomyPolicyWorkspaceWrite:
		return AutonomyPolicyWorkspaceWrite, nil
	case AutonomyPolicyDangerFullAccess:
		return AutonomyPolicyDangerFullAccess, nil
	default:
		return "", fmt.Errorf("autonomy policy %q is not supported; use auto, read-only, workspace-write, or danger-full-access", strings.TrimSpace(policy))
	}
}

func NormalizeStoredAutonomyPolicy(policy string) string {
	normalized, err := NormalizeAutonomyPolicy(policy)
	if err != nil {
		return AutonomyPolicyAuto
	}
	return normalized
}

type CodexSessionResolver interface {
	Exists(ctx context.Context, ref string) (bool, error)
}

type CodexSessionIndex struct {
	Path string
	Home string
}

type CodexAdapter struct {
	Runner          subprocess.Runner
	Dir             string
	SessionResolver CodexSessionResolver
}

func (a CodexAdapter) Name() string { return CodexRuntime }

func (a CodexAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	result, err := a.runner().Run(ctx, a.Dir, "codex", "exec", "--json", "--", request.Prompt)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, commandError(result, err)
	}
	threadID, err := parseCodexStartedThreadID(result.Stdout)
	if err != nil {
		return StartResult{Raw: result.Stdout}, err
	}
	return StartResult{RuntimeRef: threadID, Raw: result.Stdout}, nil
}

func (a CodexAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a CodexAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	if err := a.verifySession(ctx, agent); err != nil {
		return Result{}, err
	}
	args := []string{"exec", "resume"}
	if agent.RuntimeRef == "last" {
		args = append(args, "--last")
	} else {
		args = append(args, agent.RuntimeRef)
	}
	args = append(args, "--", job.Prompt)
	result, err := a.runner().Run(ctx, a.Dir, "codex", args...)
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr}, commandError(result, err)
	}
	return Result{Raw: result.Stdout}, nil
}

func (a CodexAdapter) Health(ctx context.Context, agent Agent) error {
	_, err := a.Deliver(ctx, agent, Job{Prompt: healthPrompt})
	return err
}

func (a CodexAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a CodexAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.ExecRunner{}
}

func (a CodexAdapter) verifySession(ctx context.Context, agent Agent) error {
	if agent.RuntimeRef == "last" {
		return nil
	}
	exists, err := a.sessionResolver().Exists(ctx, agent.RuntimeRef)
	if err != nil {
		return fmt.Errorf("verify codex session: %w", err)
	}
	if !exists {
		if isUUID(agent.RuntimeRef) {
			return nil
		}
		return fmt.Errorf("codex session %q was not found", agent.RuntimeRef)
	}
	return nil
}

func (a CodexAdapter) sessionResolver() CodexSessionResolver {
	if a.SessionResolver != nil {
		return a.SessionResolver
	}
	return CodexSessionIndex{}
}

type ClaudeAdapter struct {
	Runner        subprocess.Runner
	Dir           string
	NewRuntimeRef func() (string, error)
}

func (a ClaudeAdapter) Name() string { return ClaudeRuntime }

func (a ClaudeAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	runtimeRef, err := a.newRuntimeRef()
	if err != nil {
		return StartResult{}, err
	}
	args := []string{"--session-id", runtimeRef, "-p", "--output-format", "json", "--", request.Prompt}
	result, err := a.runner().Run(ctx, a.Dir, "claude", args...)
	if err != nil {
		return StartResult{Raw: result.Stdout + result.Stderr}, commandError(result, err)
	}
	return StartResult{RuntimeRef: runtimeRef, Raw: result.Stdout}, nil
}

func (a ClaudeAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a ClaudeAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	args := claudeArgs(agent, job.Prompt, true)
	result, err := a.runner().Run(ctx, a.Dir, "claude", args...)
	if err != nil && isClaudeJSONUnsupported(result) {
		result, err = a.runner().Run(ctx, a.Dir, "claude", claudeArgs(agent, job.Prompt, false)...)
	}
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr}, commandError(result, err)
	}
	parsed := Result{Raw: result.Stdout}
	var payload struct {
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(result.Stdout), &payload) == nil && payload.Result != "" {
		parsed.Summary = payload.Result
	} else {
		parsed.Summary = strings.TrimSpace(result.Stdout)
	}
	return parsed, nil
}

func (a ClaudeAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	_, err := a.Deliver(ctx, agent, Job{Prompt: healthPrompt})
	return err
}

func (a ClaudeAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a ClaudeAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.ExecRunner{}
}

func (a ClaudeAdapter) newRuntimeRef() (string, error) {
	if a.NewRuntimeRef != nil {
		return a.NewRuntimeRef()
	}
	return newUUID()
}

type ShellAdapter struct {
	Runner subprocess.Runner
	Dir    string
}

func (a ShellAdapter) Name() string { return ShellRuntime }

func (a ShellAdapter) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if err := validateStartRequest(request.Agent, a.Name(), request.Prompt); err != nil {
		return StartResult{}, err
	}
	return StartResult{}, errors.New("shell runtime does not support agent start; use gitmoot agent subscribe --runtime shell --session <command>")
}

func (a ShellAdapter) Validate(_ context.Context, agent Agent) error {
	return validateRuntime(agent, a.Name())
}

func (a ShellAdapter) Deliver(ctx context.Context, agent Agent, job Job) (Result, error) {
	if err := a.Validate(ctx, agent); err != nil {
		return Result{}, err
	}
	result, err := a.runner().Run(ctx, a.Dir, "sh", "-c", agent.RuntimeRef, "gitmoot", job.Prompt)
	if err != nil {
		return Result{Raw: result.Stdout + result.Stderr}, commandError(result, err)
	}
	return Result{Raw: result.Stdout, Summary: strings.TrimSpace(result.Stdout)}, nil
}

func (a ShellAdapter) Health(ctx context.Context, agent Agent) error {
	if err := a.Validate(ctx, agent); err != nil {
		return err
	}
	result, err := a.runner().Run(ctx, a.Dir, "sh", "-c", agent.RuntimeRef, "gitmoot-health", healthPrompt)
	if err != nil {
		return commandError(result, err)
	}
	return nil
}

func (a ShellAdapter) Capabilities(context.Context) ([]string, error) {
	return []string{"review", "implement", "ask"}, nil
}

func (a ShellAdapter) runner() subprocess.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return subprocess.ExecRunner{}
}

func commandError(result subprocess.Result, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}

func parseCodexStartedThreadID(output string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "thread.started" && strings.TrimSpace(event.ThreadID) != "" {
			return strings.TrimSpace(event.ThreadID), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("parse codex start output: %w", err)
	}
	return "", errors.New("codex start did not emit thread.started thread_id")
}

func newUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := make([]byte, 32)
	hex.Encode(encoded, bytes[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

func claudeArgs(agent Agent, prompt string, jsonOutput bool) []string {
	args := []string{}
	if agent.RuntimeRef == "last" {
		args = append(args, "--continue")
	} else {
		args = append(args, "--resume", agent.RuntimeRef)
	}
	args = append(args, "-p")
	if jsonOutput {
		args = append(args, "--output-format", "json")
	}
	return append(args, "--", prompt)
}

func isClaudeJSONUnsupported(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "output-format") &&
		(strings.Contains(text, "unknown") || strings.Contains(text, "unsupported") || strings.Contains(text, "invalid"))
}

func validateRuntime(agent Agent, runtimeName string) error {
	if err := ValidateAgent(agent); err != nil {
		return err
	}
	if agent.Runtime != runtimeName {
		return fmt.Errorf("agent runtime %q does not match adapter %q", agent.Runtime, runtimeName)
	}
	return nil
}

func validRepoScope(scope string) bool {
	parts := strings.Split(scope, "/")
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return false
			}
		}
	}
	return true
}

func (r CodexSessionIndex) Exists(ctx context.Context, ref string) (bool, error) {
	locations, err := r.locations()
	if err != nil {
		return false, err
	}
	for _, location := range locations {
		found, err := codexSessionIndexContains(ctx, location.IndexPath, ref)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
		if isUUID(ref) {
			found, err = codexShellSnapshotsContain(ctx, location.SnapshotsDir, ref)
			if err != nil {
				return false, err
			}
			if found {
				return true, nil
			}
		}
	}
	return false, nil
}

type codexSessionLocation struct {
	IndexPath    string
	SnapshotsDir string
}

func codexSessionIndexContains(ctx context.Context, path string, ref string) (bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var entry struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return false, fmt.Errorf("parse codex session index: %w", err)
		}
		if entry.ID == ref || entry.ThreadName == ref {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func codexShellSnapshotsContain(ctx context.Context, dir string, ref string) (bool, error) {
	pattern := filepath.Join(dir, ref+".*.sh")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r CodexSessionIndex) locations() ([]codexSessionLocation, error) {
	if r.Path != "" {
		return []codexSessionLocation{codexLocationForIndex(r.Path)}, nil
	}
	homes := []string{}
	if home := strings.TrimSpace(r.Home); home != "" {
		return []codexSessionLocation{codexLocationForHome(home)}, nil
	}
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		homes = append(homes, home)
	}
	userHome, err := os.UserHomeDir()
	if err != nil && len(homes) == 0 {
		return nil, err
	}
	if err == nil {
		homes = append(homes, filepath.Join(userHome, ".codex"))
	}
	locations := make([]codexSessionLocation, 0, len(homes))
	seen := map[string]bool{}
	for _, home := range homes {
		home = filepath.Clean(home)
		if seen[home] {
			continue
		}
		seen[home] = true
		locations = append(locations, codexLocationForHome(home))
	}
	return locations, nil
}

func (r CodexSessionIndex) path() (string, error) {
	locations, err := r.locations()
	if err != nil {
		return "", err
	}
	if len(locations) == 0 {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, ".codex", "session_index.jsonl"), nil
	}
	return locations[0].IndexPath, nil
}

func codexLocationForIndex(path string) codexSessionLocation {
	home := filepath.Dir(path)
	return codexSessionLocation{
		IndexPath:    path,
		SnapshotsDir: filepath.Join(home, "shell_snapshots"),
	}
}

func codexLocationForHome(home string) codexSessionLocation {
	return codexSessionLocation{
		IndexPath:    filepath.Join(home, "session_index.jsonl"),
		SnapshotsDir: filepath.Join(home, "shell_snapshots"),
	}
}
