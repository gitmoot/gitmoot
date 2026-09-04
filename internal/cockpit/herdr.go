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
	Result struct {
		Type string `json:"type"`
	} `json:"result"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// herdrWakeTimeoutMS bounds herdr's --wait on the herdr side so a delivered wake
// never blocks indefinitely (herdr's settled-state wait is unbounded without a
// --timeout) and this client's context never has to kill it mid-response. It MUST
// exceed herdr's 5000ms prompt-effect window: a --timeout at or below that window
// makes herdr report a genuine non-response as a plain `timeout` instead of
// `agent_prompt_stalled`, erasing the stall signal a wake depends on.
const herdrWakeTimeoutMS = 8000

// agentPrompt runs `herdr agent prompt <pane> <prompt> --wait --timeout N`,
// optionally with `--until <until>`. Outcomes, from herdr's contract:
//   - result.type=agent_prompted (stdout): delivered.
//   - error.code=timeout (stderr, non-zero exit): delivery WAS observed but the
//     agent had not reached a settled state within the window. For a wake that is
//     a successful landing — we never wait for the woken agent to finish — so it
//     counts as delivered.
//   - error.code=agent_prompt_stalled (stderr, non-zero exit): submitted but no
//     state change in herdr's effect window: not delivered, but a normal outcome,
//     not a transport error.
//
// It reads the combined stream so the stderr envelopes above stay parseable.
func (c herdrClient) agentPrompt(ctx context.Context, pane, prompt, until string) (delivered bool, stalled bool, err error) {
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
		if runErr != nil {
			return false, false, fmt.Errorf("agent prompt failed: %w (parse response: %v)", runErr, err)
		}
		return false, false, fmt.Errorf("parse agent prompt response: %w", err)
	}
	switch response.Error.Code {
	case "agent_prompt_stalled":
		return false, true, nil
	case "timeout":
		return true, false, nil
	}
	if runErr != nil {
		if response.Error.Code != "" {
			return false, false, fmt.Errorf("agent prompt %s: %s: %w", response.Error.Code, response.Error.Message, runErr)
		}
		return false, false, fmt.Errorf("agent prompt failed: %w", runErr)
	}
	if response.Result.Type != "agent_prompted" {
		return false, false, fmt.Errorf("agent prompt returned result type %q", response.Result.Type)
	}
	return true, false, nil
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

// resolvePaneByLabel returns the CURRENT pane id matching a literal pane id or
// exact label. It is resolved at call (wake) time: ids stay pinned to one live
// pane, while labels follow whichever live pane uniquely has that cosmetic
// value. Stale ids, absent labels, and ambiguous labels fail.
func (c herdrClient) resolvePaneByLabel(ctx context.Context, binding string) (string, bool, error) {
	binding = strings.TrimSpace(binding)
	if binding == "" {
		return "", false, nil
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
			return p.PaneID, true, nil
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
	return resolved, resolved != "", nil
}
