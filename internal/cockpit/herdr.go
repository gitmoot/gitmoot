package cockpit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

type agentPromptResult struct {
	ID     string `json:"id"`
	Result struct {
		Type     string `json:"type"`
		Delivery string `json:"delivery"`
	} `json:"result"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Bound both submission observation and the optional settled-state wait.
const herdrWakeTimeoutMS = 8000

// agentPrompt distinguishes confirmed submission, uncertain delivery, and a
// proven rejection before input. Uncertain input must never be blindly retried.
// A confirmed submission does not imply that the recipient has read or acted.
func (c herdrClient) agentPrompt(ctx context.Context, pane, prompt, until string) (delivered bool, uncertain bool, err error) {
	args := []string{"agent", "prompt", pane, prompt, "--wait", "--timeout", strconv.Itoa(herdrWakeTimeoutMS)}
	if strings.TrimSpace(until) != "" {
		args = append(args, "--until", strings.TrimSpace(until))
	}
	run := c.runCombined
	if run == nil {
		run = c.run
	}
	out, runErr := run(ctx, args...)
	var response agentPromptResult
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return false, true, fmt.Errorf("agent prompt receipt unreadable: %w (transport: %v)", err, runErr)
	}
	if response.Error.Code == "agent_status_unobserved_after_submit" ||
		(response.Error.Code == "" && response.Result.Type == "agent_prompted" && response.Result.Delivery == "submitted") {
		return true, false, nil
	}
	detail := fmt.Errorf("agent prompt receipt %q code=%q delivery=%q (transport: %v)",
		response.ID, response.Error.Code, response.Result.Delivery, runErr)
	switch response.Error.Code {
	case "agent_not_found", "agent_blocked", "agent_input_pending":
		// These are explicit pre-write refusals. Do not classify by error prose.
		return false, false, detail
	default:
		// Includes written_to_pty, legacy stalled, unsubmitted drafts, timeouts,
		// missing delivery evidence and errors after a possibly successful write.
		return false, true, detail
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
	Name             string          `json:"name"`
	PaneID           string          `json:"pane_id"`
	MachineID        string          `json:"machine_id"`
	MachineProfileID string          `json:"machine_profile_id"`
	Archived         json.RawMessage `json:"archived"`
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

func registeredRecipient(agents []registeredAgent, binding string) (string, bool) {
	name, byName := strings.CutPrefix(binding, "agent:")
	resolved := ""
	for _, agent := range agents {
		if agent.PaneID == "" || (len(agent.Archived) != 0 && string(agent.Archived) != "null") {
			continue
		}
		match := agent.PaneID == binding
		if byName {
			match = name != "" && agent.Name == name && agent.MachineID == "" && agent.MachineProfileID == ""
		}
		if match {
			if resolved != "" {
				return "", false
			}
			resolved = agent.PaneID
		}
	}
	return resolved, resolved != ""
}

// Explicit agent:name bindings follow registered local seats across terminal
// replacement. Legacy pane ids/labels remain pinned, but must name an agent.
func (c herdrClient) resolvePaneByLabel(ctx context.Context, binding string) (string, bool, error) {
	binding = strings.TrimSpace(binding)
	if binding == "" {
		return "", false, nil
	}
	agents, err := c.registeredAgents(ctx)
	if err != nil {
		return "", false, err
	}
	if strings.HasPrefix(binding, "agent:") {
		pane, ok := registeredRecipient(agents, binding)
		return pane, ok, nil
	}
	out, err := c.run(ctx, "pane", "list")
	if err != nil {
		return "", false, err
	}
	var pl paneListResult
	if err := json.Unmarshal([]byte(out), &pl); err != nil {
		return "", false, fmt.Errorf("parse pane list: %w", err)
	}
	for _, p := range pl.Result.Panes {
		if p.PaneID == binding && p.PaneID != "" {
			pane, ok := registeredRecipient(agents, p.PaneID)
			return pane, ok, nil
		}
	}
	resolved := ""
	for _, p := range pl.Result.Panes {
		if p.PaneID != "" && p.Label == binding {
			if resolved != "" {
				return "", false, nil
			}
			resolved = p.PaneID
		}
	}
	pane, ok := registeredRecipient(agents, resolved)
	return pane, ok, nil
}
