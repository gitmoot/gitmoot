package cockpit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runner executes a single herdr CLI invocation and returns its stdout. It is
// injectable so tests can drive the client with a fake (no real herdr server).
// The default runner (newExecRunner) execs the configured herdr binary.
type runner func(ctx context.Context, args ...string) (stdout string, err error)

// newExecRunner returns a runner that execs the herdr binary at bin. When the
// caller sets HERDR_SOCKET_PATH, it is passed through to the child process so
// the spike's reachability gating (a background/daemon context reaching the
// single herdr server) holds; an unset value defaults to herdr's own socket.
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

// workspaceResult mirrors `herdr workspace create`: it carries the new
// workspace id and the id of its root pane (the parent for the first split).
type workspaceResult struct {
	Result struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	} `json:"result"`
}

// workspaceCreate runs `herdr workspace create --cwd <dir> --label <label>
// --no-focus` and returns (workspaceID, rootPaneID).
func (c herdrClient) workspaceCreate(ctx context.Context, cwd, label string) (workspaceID string, rootPaneID string, err error) {
	out, err := c.run(ctx, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus")
	if err != nil {
		return "", "", err
	}
	var ws workspaceResult
	if err := json.Unmarshal([]byte(out), &ws); err != nil {
		return "", "", fmt.Errorf("parse workspace create: %w", err)
	}
	id := ws.Result.Workspace.WorkspaceID
	root := ws.Result.RootPane.PaneID
	if id == "" || root == "" {
		return "", "", fmt.Errorf("workspace create returned empty ids")
	}
	return id, root, nil
}

// paneResult mirrors `herdr pane split`: the new pane's id.
type paneResult struct {
	Result struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	} `json:"result"`
}

// paneSplit runs `herdr pane split <parent> --direction down --cwd <worktree>
// --no-focus` and returns the new child pane id.
func (c herdrClient) paneSplit(ctx context.Context, parentPane, cwd string) (paneID string, err error) {
	out, err := c.run(ctx, "pane", "split", parentPane, "--direction", "down", "--cwd", cwd, "--no-focus")
	if err != nil {
		return "", err
	}
	var p paneResult
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return "", fmt.Errorf("parse pane split: %w", err)
	}
	if p.Result.Pane.PaneID == "" {
		return "", fmt.Errorf("pane split returned empty pane id")
	}
	return p.Result.Pane.PaneID, nil
}

// paneRename runs `herdr pane rename <pane> "<label>"`.
func (c herdrClient) paneRename(ctx context.Context, pane, label string) error {
	_, err := c.run(ctx, "pane", "rename", pane, label)
	return err
}

// reportAgent runs `herdr pane report-agent <pane> --source <source> --agent
// <agent> --state <state>`.
func (c herdrClient) reportAgent(ctx context.Context, pane, source, agent, state string) error {
	_, err := c.run(ctx, "pane", "report-agent", pane, "--source", source, "--agent", agent, "--state", state)
	return err
}

// paneRun runs `herdr pane run <pane> "<command>"`. The command is a single
// shell string (e.g. the gitmoot job-watch invocation) handed to the pane.
func (c herdrClient) paneRun(ctx context.Context, pane, command string) error {
	_, err := c.run(ctx, "pane", "run", pane, command)
	return err
}

// releaseAgent runs `herdr pane release-agent <pane> --source <source> --agent
// <agent>`.
func (c herdrClient) releaseAgent(ctx context.Context, pane, source, agent string) error {
	_, err := c.run(ctx, "pane", "release-agent", pane, "--source", source, "--agent", agent)
	return err
}

// paneClose runs `herdr pane close <pane>`.
func (c herdrClient) paneClose(ctx context.Context, pane string) error {
	_, err := c.run(ctx, "pane", "close", pane)
	return err
}

// workspaceClose runs `herdr workspace close <workspace>`.
func (c herdrClient) workspaceClose(ctx context.Context, workspace string) error {
	_, err := c.run(ctx, "workspace", "close", workspace)
	return err
}

// workspaceFocus runs `herdr workspace focus <workspace>` to bring a workspace
// forward (Task 8 reconvene). herdr has no focus-pane-by-id verb (only the
// directional `pane focus`), so workspace focus is the closest supported gesture.
func (c herdrClient) workspaceFocus(ctx context.Context, workspace string) error {
	_, err := c.run(ctx, "workspace", "focus", workspace)
	return err
}

// paneListResult mirrors `herdr pane list`: only each pane's id is load-bearing
// for the reconcile GC (which pane rows still have a live herdr pane).
type paneListResult struct {
	Result struct {
		Panes []struct {
			PaneID string `json:"pane_id"`
		} `json:"panes"`
	} `json:"result"`
}

// paneList runs `herdr pane list` and returns every live pane id. It backs the
// reconcile sweep, which drops cockpit_pane rows whose pane is no longer present.
func (c herdrClient) paneList(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "pane", "list")
	if err != nil {
		return nil, err
	}
	var pl paneListResult
	if err := json.Unmarshal([]byte(out), &pl); err != nil {
		return nil, fmt.Errorf("parse pane list: %w", err)
	}
	ids := make([]string, 0, len(pl.Result.Panes))
	for _, p := range pl.Result.Panes {
		if p.PaneID != "" {
			ids = append(ids, p.PaneID)
		}
	}
	return ids, nil
}

// shortJobID trims a job id to the 8-char form used in the gm-<jobid8> agent
// label (the spike's verified report-agent --agent convention).
func shortJobID(jobID string) string {
	jobID = strings.TrimSpace(jobID)
	if len(jobID) > 8 {
		return jobID[:8]
	}
	return jobID
}
