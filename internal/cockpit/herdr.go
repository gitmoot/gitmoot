package cockpit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runner executes a single herdr CLI invocation and returns its output. It is
// injectable so tests can drive the client with a fake (no real herdr server).
// The default runner (newExecRunner) execs the configured herdr binary.
type runner func(ctx context.Context, args ...string) (output string, err error)

// newExecRunner returns a runner that execs the herdr binary at bin and returns
// its STDOUT only. When the caller sets HERDR_SOCKET_PATH, it is passed through
// to the child process so the spike's reachability gating (a background/daemon
// context reaching the single herdr server) holds; an unset value defaults to
// herdr's own socket. Every verb but agentPrompt uses this stdout-only runner so
// a stray herdr stderr line can never corrupt a success-path JSON parse.
func newExecRunner(bin string) runner {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		// Inherit the daemon environment (incl. HERDR_SOCKET_PATH when set) so
		// herdr resolves the same socket the reachability check used.
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		return string(out), err
	}
}

// newExecRunnerCombined is newExecRunner but returns COMBINED stdout+stderr. Only
// agentPrompt uses it: herdr writes its delivery-outcome envelopes
// (agent_prompt_stalled / timeout) to stderr on a non-zero exit, so the combined
// stream keeps them parseable. Scoping the merge to this one verb leaves every
// other verb on the stdout-only runner above.
func newExecRunnerCombined(bin string) runner {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

// herdrClient is a thin, typed wrapper over the verified herdr CLI surface. It
// owns no state beyond the runner and binary name; every call is a one-shot
// invocation. JSON parsing targets only the fields the spike verified.
type herdrClient struct {
	run runner
	// runCombined runs the one verb (agentPrompt) that must read herdr's stderr
	// error envelope. It falls back to run when nil so a test that injects only
	// run still drives agentPrompt.
	runCombined runner
	bin         string
	// lookPath resolves the herdr binary on PATH; injectable so tests can drive
	// availability deterministically without a real herdr install.
	lookPath func(string) (string, error)
}

// status mirrors the shape of `herdr status --json`. Only the running flag is
// load-bearing for gating; the rest is decoded best-effort.
type statusResult struct {
	Server struct {
		Running bool `json:"running"`
	} `json:"server"`
}

// available reports whether the herdr binary is on PATH and the server is
// reachable (`herdr status --json` rc 0 + .server.running == true).
func (c herdrClient) available(ctx context.Context) bool {
	if c.bin == "" {
		return false
	}
	lookPath := c.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath(c.bin); err != nil {
		return false
	}
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return false
	}
	var st statusResult
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return false
	}
	return st.Server.Running
}

type agentNotificationResult struct {
	ID     string `json:"id"`
	Result struct {
		Type    string `json:"type"`
		Outcome struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"outcome"`
	} `json:"result"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// NotificationDeferred proves that runtime admission did not happen.
// The durable obligation remains pending without spending delivery retry budget.
type NotificationDeferred struct {
	Reason string
}

func (e *NotificationDeferred) Error() string {
	return "notification deferred: " + e.Reason
}

// NotificationTarget carries the runtime identity observed during resolution.
// The selector alone is mutable and cannot pin a later admission request.
type NotificationTarget struct {
	Selector string
	Runtime  *NotificationRuntime
}

type NotificationRuntime struct {
	RuntimeID  string `json:"runtimeId"`
	SessionID  string `json:"sessionId"`
	Generation uint64 `json:"generation"`
}

// agentNotify never types into a pane or waits for a running turn to settle.
// A missing receipt is unknown, even when the command exits unsuccessfully.
func (c herdrClient) agentNotify(ctx context.Context, target NotificationTarget, prompt string) (delivered bool, uncertain bool, err error) {
	if target.Runtime == nil || target.Runtime.RuntimeID == "" || target.Runtime.SessionID == "" {
		return false, false, &NotificationDeferred{Reason: "runtime notification capability unavailable"}
	}
	expected, encodeErr := json.Marshal(target.Runtime)
	if encodeErr != nil {
		return false, false, &NotificationDeferred{Reason: encodeErr.Error()}
	}
	run := c.runCombined
	if run == nil {
		run = c.run
	}
	out, runErr := run(ctx, "agent", "prompt-safe", target.Selector, prompt, "--expected-target", string(expected))
	var response agentNotificationResult
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return false, true, fmt.Errorf("notification receipt unreadable: %w (transport: %v)", err, runErr)
	}
	if response.Error.Code == "" && response.Result.Type == "agent_prompt_safe" {
		switch response.Result.Outcome.Status {
		case "accepted":
			return true, false, nil
		case "deferred":
			return false, false, &NotificationDeferred{Reason: response.Result.Outcome.Reason}
		}
	}
	switch response.Error.Code {
	case "agent_not_found", "invalid_notification", "method_not_found":
		return false, false, &NotificationDeferred{Reason: response.Error.Code}
	default:
		return false, true, fmt.Errorf("notification receipt %q code=%q status=%q (transport: %v)",
			response.ID, response.Error.Code, response.Result.Outcome.Status, runErr)
	}
}

// paneListResult mirrors `herdr pane list`: only each pane's id is load-bearing
// for the reconcile GC (which pane rows still have a live herdr pane).
type paneListResult struct {
	Result struct {
		Panes []struct {
			PaneID string `json:"pane_id"`
			Label  string `json:"label"`
		} `json:"panes"`
	} `json:"result"`
}

type registeredAgent struct {
	Name               string               `json:"name"`
	PaneID             string               `json:"pane_id"`
	MachineID          string               `json:"machine_id"`
	MachineProfileID   string               `json:"machine_profile_id"`
	Archived           json.RawMessage      `json:"archived"`
	NotificationTarget *NotificationRuntime `json:"notification_target"`
}

func (c herdrClient) registeredAgents(ctx context.Context) ([]registeredAgent, error) {
	out, err := c.run(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var result struct {
		Result struct {
			Agents []registeredAgent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("parse agent list: %w", err)
	}
	if result.Result.Agents == nil {
		return nil, fmt.Errorf("agent list is missing registered recipients")
	}
	return result.Result.Agents, nil
}

func registeredAgentForBinding(agents []registeredAgent, binding string) (*registeredAgent, bool) {
	name, byName := strings.CutPrefix(binding, "agent:")
	var resolved *registeredAgent
	for i := range agents {
		agent := &agents[i]
		if agent.PaneID == "" || agent.MachineID != "" || agent.MachineProfileID != "" ||
			(len(agent.Archived) != 0 && string(agent.Archived) != "null") {
			continue
		}
		match := agent.PaneID == binding
		if byName {
			match = name != "" && agent.Name == name
		}
		if match {
			if resolved != nil {
				return nil, false
			}
			resolved = agent
		}
	}
	return resolved, resolved != nil
}

func registeredRecipient(agents []registeredAgent, binding string) (string, bool) {
	agent, ok := registeredAgentForBinding(agents, binding)
	if !ok {
		return "", false
	}
	return agent.PaneID, true
}

func (c herdrClient) resolvePaneByLabel(ctx context.Context, binding string) (string, bool, error) {
	agent, ok, err := c.resolveRegisteredRecipient(ctx, binding)
	if !ok || err != nil {
		return "", false, err
	}
	return agent.PaneID, true, nil
}

// Explicit agent:name bindings follow registered local seats across terminal
// replacement. Legacy pane ids/labels remain pinned, but must name an agent.
func (c herdrClient) resolveRegisteredRecipient(ctx context.Context, binding string) (*registeredAgent, bool, error) {
	binding = strings.TrimSpace(binding)
	if binding == "" {
		return nil, false, nil
	}
	agents, err := c.registeredAgents(ctx)
	if err != nil {
		return nil, false, err
	}
	if strings.HasPrefix(binding, "agent:") {
		agent, ok := registeredAgentForBinding(agents, binding)
		return agent, ok, nil
	}
	out, err := c.run(ctx, "pane", "list")
	if err != nil {
		return nil, false, err
	}
	var pl paneListResult
	if err := json.Unmarshal([]byte(out), &pl); err != nil {
		return nil, false, fmt.Errorf("parse pane list: %w", err)
	}
	for _, p := range pl.Result.Panes {
		if p.PaneID == binding && p.PaneID != "" {
			agent, ok := registeredAgentForBinding(agents, p.PaneID)
			return agent, ok, nil
		}
	}
	resolved := ""
	for _, p := range pl.Result.Panes {
		if p.PaneID != "" && p.Label == binding {
			if resolved != "" {
				return nil, false, nil
			}
			resolved = p.PaneID
		}
	}
	agent, ok := registeredAgentForBinding(agents, resolved)
	return agent, ok, nil
}
