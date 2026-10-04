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
// herdr's own socket. Only stdout is returned so a stray herdr stderr line can
// never corrupt a success-path JSON parse.
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

// herdrClient is a thin, typed wrapper over the verified herdr CLI surface. It
// owns no state beyond the runner and binary name; every call is a one-shot
// invocation. JSON parsing targets only the fields the spike verified.
type herdrClient struct {
	run runner
	bin string
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

// NotificationDeferred proves that runtime admission did not happen.
// The durable obligation remains pending without spending delivery retry budget.
type NotificationDeferred struct {
	Reason string
}

func (e *NotificationDeferred) Error() string {
	return "notification deferred: " + e.Reason
}

// NotificationTarget is the recipient resolved from the live Herdr registry.
// Delivery re-reads the pane's foreground processes, so a replaced runtime is
// matched to its own add-on registration rather than to this snapshot.
type NotificationTarget struct {
	Selector string
	PaneID   string
	// Kind is Herdr's detected agent kind ("omp", "claude", "codex", ...).
	Kind string
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
	Agent            string          `json:"agent"`
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
