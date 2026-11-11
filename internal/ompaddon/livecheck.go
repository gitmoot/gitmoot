package ompaddon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed livecheck-model.ts
var livecheckModel []byte

// LiveStep is one result of LiveCheck.
type LiveStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// LiveCheck starts ompBinary in a throwaway interactive session (its own
// pseudo-terminal, agent directory and registry directory, with a local
// stand-in model) carrying the add-on this binary installs. It checks that the
// session registers, answers a probe, accepts an idle delivery that reaches
// the model, delivers past a queue OMP left stranded, defers while someone
// types, refuses a stale target, and removes its registration on shutdown.
// Nothing outside the temporary directory is touched, and the real Gitmoot
// registry never sees the session.
func LiveCheck(ctx context.Context, ompBinary string) []LiveStep {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run := &liveRun{}
	steps := run.check(ctx, ompBinary)
	run.cleanup()
	return steps
}

type liveRun struct {
	dir    string
	cmd    *exec.Cmd
	pty    *os.File
	output tailBuffer
	exited chan struct{}
}

type registration struct {
	Version   int    `json:"version"`
	RuntimeID string `json:"runtimeId"`
	SessionID string `json:"sessionId"`
	Gen       int    `json:"generation"`
	PID       int    `json:"pid"`
	Endpoint  string `json:"endpoint"`
}

func (r *liveRun) check(ctx context.Context, ompBinary string) []LiveStep {
	var steps []LiveStep
	fail := func(name, format string, args ...any) []LiveStep {
		return append(steps, LiveStep{Name: name, Detail: fmt.Sprintf(format, args...)})
	}
	pass := func(name, format string, args ...any) {
		steps = append(steps, LiveStep{Name: name, OK: true, Detail: fmt.Sprintf(format, args...)})
	}

	reg, err := r.start(ctx, ompBinary)
	if err != nil {
		return fail("session", "%v%s", err, r.output.excerpt())
	}
	pass("session", "omp pid %d registered runtime %s", reg.PID, reg.RuntimeID)

	target := map[string]any{"runtimeId": reg.RuntimeID, "sessionId": reg.SessionID, "generation": reg.Gen}
	reply, err := call(reg.Endpoint, map[string]any{"v": 1, "id": "probe", "method": "probe"})
	if err != nil {
		return fail("probe", "%v", err)
	}
	if reply["status"] != "ok" || !sameJSON(reply["target"], target) {
		return fail("probe", "unexpected reply %s", compactJSON(reply))
	}
	pass("probe", "target matches registration")

	if err := waitFor(ctx, 20*time.Second, func() (bool, error) { return idleAdmission(reg.Endpoint) }); err != nil {
		return fail("idle-delivery", "session never became ready for an idle delivery: %v", err)
	}
	token := "gitmoot-live-check-" + randomHex(6)
	reply, err = call(reg.Endpoint, map[string]any{"v": 1, "id": "deliver", "method": "deliver", "target": target,
		"content": "Gitmoot plugin doctor live check " + token + "; nothing to do."})
	if err != nil {
		return fail("idle-delivery", "%v", err)
	}
	if reply["status"] != "accepted" || reply["mode"] != "idle" {
		return fail("idle-delivery", "unexpected reply %s", compactJSON(reply))
	}
	logPath := filepath.Join(r.dir, "model.jsonl")
	if err := waitFor(ctx, 30*time.Second, func() (bool, error) {
		data, err := os.ReadFile(logPath)
		return err == nil && bytes.Contains(data, []byte(token)), nil
	}); err != nil {
		return fail("idle-delivery", "accepted, but the note never reached the model: %v", err)
	}
	if err := waitFor(ctx, 20*time.Second, func() (bool, error) { return idleAdmission(reg.Endpoint) }); err != nil {
		return fail("idle-delivery", "note reached the model, but the turn did not settle: %v", err)
	}
	pass("idle-delivery", "accepted while idle; the note started a turn and reached the model")

	detail, err := r.checkStuckQueue(ctx, reg.Endpoint, target, logPath)
	if err != nil {
		return fail("stuck-queue", "%v", err)
	}
	pass("stuck-queue", "%s", detail)

	if _, err := r.pty.Write([]byte("x")); err != nil {
		return fail("deferral", "type into the test session: %v", err)
	}
	if err := waitFor(ctx, 5*time.Second, func() (bool, error) {
		reply, err := call(reg.Endpoint, map[string]any{"v": 1, "id": "probe", "method": "probe"})
		if err != nil {
			return false, err
		}
		state, _ := reply["state"].(map[string]any)
		return state != nil && state["msSinceKey"] != nil, nil
	}); err != nil {
		return fail("deferral", "the add-on did not see the typed key: %v", err)
	}
	reply, err = call(reg.Endpoint, map[string]any{"v": 1, "id": "typing", "method": "deliver", "target": target, "content": "must wait"})
	if err != nil {
		return fail("deferral", "%v", err)
	}
	if reply["status"] != "deferred" || reply["reason"] != "operator_active" {
		return fail("deferral", "expected deferred/operator_active while typing, got %s", compactJSON(reply))
	}
	pass("deferral", "deferred while the operator types (operator_active)")

	stale := map[string]any{"runtimeId": reg.RuntimeID, "sessionId": reg.SessionID, "generation": reg.Gen + 1}
	reply, err = call(reg.Endpoint, map[string]any{"v": 1, "id": "stale", "method": "deliver", "target": stale, "content": "must not land"})
	if err != nil {
		return fail("stale-session", "%v", err)
	}
	if reply["status"] != "deferred" || reply["reason"] != "stale_session" {
		return fail("stale-session", "expected deferred/stale_session, got %s", compactJSON(reply))
	}
	pass("stale-session", "refused a target from another session generation")

	if err := r.stop(); err != nil {
		return fail("shutdown", "%v", err)
	}
	regPath := filepath.Join(r.dir, "run", reg.RuntimeID+".json")
	for _, leftover := range []string{regPath, reg.Endpoint} {
		if _, err := os.Lstat(leftover); err == nil {
			return fail("shutdown", "%s still exists after omp exited", leftover)
		}
	}
	pass("shutdown", "registration and socket removed when omp exited")
	return steps
}

// liveQueueGrace and liveInactivity replace the add-on's 60 s queue grace and
// 10 min operator inactivity in the live session.
const (
	liveQueueGrace = 5 * time.Second
	liveInactivity = 15 * time.Second
)

// checkStuckQueue has the stand-in model leave the session idle with a queued
// follow-up OMP does not drain by itself. Within the queue grace the add-on
// defers. After the grace a key the operator pressed still defers it. Once the
// operator has also been inactive long enough, a note is accepted idle, its
// turn reaches the model and takes the follow-up with it, and the queue is
// empty.
func (r *liveRun) checkStuckQueue(ctx context.Context, endpoint string, target map[string]any, logPath string) (string, error) {
	queued := "gitmoot-live-check-queued-" + randomHex(6)
	control := filepath.Join(r.dir, "control")
	if err := os.WriteFile(control, []byte("Gitmoot plugin doctor live check follow-up "+queued+"; nothing to do."), 0o600); err != nil {
		return "", err
	}
	var stranded struct {
		Idle            bool   `json:"idle"`
		PendingMessages bool   `json:"pendingMessages"`
		Error           string `json:"error"`
	}
	if err := waitFor(ctx, 20*time.Second, func() (bool, error) {
		data, err := os.ReadFile(control + ".done")
		if err != nil {
			return false, nil
		}
		return true, json.Unmarshal(data, &stranded)
	}); err != nil {
		return "", fmt.Errorf("queue a follow-up behind an interrupted turn: %w", err)
	}
	if stranded.Error != "" || !stranded.Idle || !stranded.PendingMessages {
		return "", fmt.Errorf("the session is not idle with a queued follow-up: %+v", stranded)
	}

	early := "gitmoot-live-check-early-" + randomHex(6)
	reply, err := call(endpoint, map[string]any{"v": 1, "id": "queued-early", "method": "deliver", "target": target,
		"content": "Gitmoot plugin doctor live check " + early + "; nothing to do."})
	if err != nil {
		return "", err
	}
	if reply["status"] != "deferred" || reply["reason"] != "busy" {
		return "", fmt.Errorf("expected deferred/busy within the queue grace, got %s", compactJSON(reply))
	}
	// The add-on started its queue clock at the latest with that request.
	firstSeen := time.Now()

	// A key and its erasure: operator activity that leaves no draft behind.
	if _, err := r.pty.Write([]byte("x\x7f")); err != nil {
		return "", fmt.Errorf("type into the test session: %w", err)
	}
	var state map[string]any
	if err := waitFor(ctx, liveQueueGrace+10*time.Second, func() (bool, error) {
		reply, err := call(endpoint, map[string]any{"v": 1, "id": "queued-key-probe", "method": "probe"})
		if err != nil {
			return false, err
		}
		state, _ = reply["state"].(map[string]any)
		return state["msSinceKey"] != nil && time.Since(firstSeen) > liveQueueGrace+500*time.Millisecond, nil
	}); err != nil {
		return "", fmt.Errorf("the add-on did not see the typed key (state %s): %w", compactJSON(state), err)
	}
	recent := "gitmoot-live-check-recent-" + randomHex(6)
	reply, err = call(endpoint, map[string]any{"v": 1, "id": "queued-recent", "method": "deliver", "target": target,
		"content": "Gitmoot plugin doctor live check " + recent + "; nothing to do."})
	if err != nil {
		return "", err
	}
	sinceKey, _ := state["msSinceKey"].(float64)
	if reply["status"] != "deferred" || (reply["reason"] != "busy" && reply["reason"] != "operator_active") {
		return "", fmt.Errorf("expected deferred/busy or operator_active %vms after a key with the queue past its grace, got %s", sinceKey, compactJSON(reply))
	}
	keyDeferral := reply["reason"]

	if err := waitFor(ctx, liveInactivity+10*time.Second, func() (bool, error) {
		reply, err := call(endpoint, map[string]any{"v": 1, "id": "queued-probe", "method": "probe"})
		if err != nil {
			return false, err
		}
		state, _ = reply["state"].(map[string]any)
		admission, _ := state["admission"].(map[string]any)
		return admission["status"] == "accepted" && admission["mode"] == "idle", nil
	}); err != nil {
		return "", fmt.Errorf("still not admitting an idle note %v after the last key (state %s): %w", liveInactivity, compactJSON(state), err)
	}
	if state["pendingMessages"] != true || state["pendingMessagesForMs"] == nil {
		return "", fmt.Errorf("probe does not report the waiting queue: %s", compactJSON(state))
	}
	queuedFor := state["pendingMessagesForMs"]
	keyAge := state["msSinceKey"]

	token := "gitmoot-live-check-late-" + randomHex(6)
	reply, err = call(endpoint, map[string]any{"v": 1, "id": "queued-late", "method": "deliver", "target": target,
		"content": "Gitmoot plugin doctor live check " + token + "; nothing to do."})
	if err != nil {
		return "", err
	}
	if reply["status"] != "accepted" || reply["mode"] != "idle" {
		return "", fmt.Errorf("expected accepted/idle after the queue grace and operator inactivity, got %s", compactJSON(reply))
	}
	if err := waitFor(ctx, 30*time.Second, func() (bool, error) {
		data, err := os.ReadFile(logPath)
		return err == nil && bytes.Contains(data, []byte(token)) && bytes.Contains(data, []byte(queued)), nil
	}); err != nil {
		return "", fmt.Errorf("accepted, but the note and the queued follow-up did not both reach the model: %w", err)
	}
	if err := waitFor(ctx, 20*time.Second, func() (bool, error) {
		reply, err := call(endpoint, map[string]any{"v": 1, "id": "queued-after", "method": "probe"})
		if err != nil {
			return false, err
		}
		state, _ = reply["state"].(map[string]any)
		return state["idle"] == true && state["pendingMessages"] == false, nil
	}); err != nil {
		return "", fmt.Errorf("the queue is not empty after the note's turn (state %s): %w", compactJSON(state), err)
	}
	if data, err := os.ReadFile(logPath); err != nil || bytes.Contains(data, []byte(early)) || bytes.Contains(data, []byte(recent)) {
		return "", fmt.Errorf("a deferred note reached the model (read error %v)", err)
	}
	return fmt.Sprintf("deferred busy within the %v queue grace; past it, deferred %v %vms after a key; once the operator was inactive %v (queue waiting %vms, last key %vms ago) accepted idle, and the note's turn took the queued follow-up to the model and emptied the queue",
		liveQueueGrace, keyDeferral, sinceKey, liveInactivity, queuedFor, keyAge), nil
}

func (r *liveRun) start(ctx context.Context, ompBinary string) (registration, error) {
	// A short temporary path keeps the socket path under the Unix limit.
	dir, err := os.MkdirTemp("", "gm-omp-")
	if err != nil {
		return registration{}, err
	}
	r.dir = dir
	registryDir := filepath.Join(dir, "run")
	agentDir := filepath.Join(dir, "agent")
	workDir := filepath.Join(dir, "work")
	modelPath := filepath.Join(dir, "livecheck-model.ts")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return registration{}, err
	}
	if err := os.WriteFile(modelPath, livecheckModel, 0o644); err != nil {
		return registration{}, err
	}
	if _, _, err := Install(filepath.Join(agentDir, "extensions"), registryDir); err != nil {
		return registration{}, err
	}

	cmd := exec.Command(ompBinary, "-e", modelPath, "--model", "gitmoot-livecheck/mock",
		"--no-tools", "--no-lsp", "--no-title", "--no-rules", "--no-skills")
	cmd.Dir = workDir
	cmd.Env = append(liveEnv(os.Environ()),
		AgentDirEnv+"="+agentDir,
		"OMP_SKIP_SETUP=1",
		"TERM=xterm-256color",
		"GITMOOT_OMP_LIVECHECK_LOG="+filepath.Join(dir, "model.jsonl"),
		"GITMOOT_OMP_LIVECHECK_CONTROL="+filepath.Join(dir, "control"),
		fmt.Sprintf("GITMOOT_OMP_INBOX_QUEUE_GRACE_MS=%d", liveQueueGrace.Milliseconds()),
		fmt.Sprintf("GITMOOT_OMP_INBOX_INACTIVITY_MS=%d", liveInactivity.Milliseconds()),
	)
	pty, err := startInPTY(cmd)
	if err != nil {
		return registration{}, fmt.Errorf("start %s: %w", ompBinary, err)
	}
	r.cmd, r.pty, r.exited = cmd, pty, make(chan struct{})
	go func() {
		_, _ = io.Copy(&r.output, pty)
	}()
	go func() {
		_ = cmd.Wait()
		close(r.exited)
	}()

	var reg registration
	err = waitFor(ctx, 60*time.Second, func() (bool, error) {
		select {
		case <-r.exited:
			return false, fmt.Errorf("omp exited before registering (%v)", cmd.ProcessState)
		default:
		}
		matches, _ := filepath.Glob(filepath.Join(registryDir, "*.json"))
		if len(matches) == 0 {
			return false, nil
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			return false, nil
		}
		if err := json.Unmarshal(data, &reg); err != nil {
			return false, fmt.Errorf("registration %s: %w", matches[0], err)
		}
		return true, nil
	})
	if err != nil {
		return reg, err
	}
	if reg.Version != 1 || reg.PID != cmd.Process.Pid || filepath.Dir(reg.Endpoint) != registryDir {
		return reg, fmt.Errorf("registration does not describe the started omp (pid %d): %+v", cmd.Process.Pid, reg)
	}
	for path, want := range map[string]os.FileMode{
		registryDir: 0o700,
		filepath.Join(registryDir, reg.RuntimeID+".json"): 0o600,
		reg.Endpoint: 0o600,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return reg, err
		}
		if info.Mode().Perm() != want {
			return reg, fmt.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	return reg, nil
}

func (r *liveRun) stop() error {
	if r.cmd == nil {
		return nil
	}
	select {
	case <-r.exited:
		return fmt.Errorf("omp exited early (%v)", r.cmd.ProcessState)
	default:
	}
	_ = r.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-r.exited:
		return nil
	case <-time.After(15 * time.Second):
		_ = r.cmd.Process.Kill()
		<-r.exited
		return errors.New("omp did not exit within 15s of SIGTERM")
	}
}

func (r *liveRun) cleanup() {
	if r.cmd != nil {
		select {
		case <-r.exited:
		default:
			_ = r.cmd.Process.Kill()
			<-r.exited
		}
	}
	if r.pty != nil {
		_ = r.pty.Close()
	}
	if r.dir != "" {
		_ = os.RemoveAll(r.dir)
	}
}

// liveEnv drops variables that would point the throwaway session at the
// operator's OMP profile, Herdr pane, or tuned add-on thresholds.
func liveEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case name == AgentDirEnv, name == "OMP_PROFILE", name == "PI_PROFILE", name == "OMPCODE", name == "TERM",
			strings.HasPrefix(name, "HERDR_"), strings.HasPrefix(name, "GITMOOT_OMP_"):
			continue
		}
		out = append(out, entry)
	}
	return out
}

func idleAdmission(endpoint string) (bool, error) {
	reply, err := call(endpoint, map[string]any{"v": 1, "id": "ready", "method": "probe"})
	if err != nil {
		return false, err
	}
	state, _ := reply["state"].(map[string]any)
	admission, _ := state["admission"].(map[string]any)
	return admission["status"] == "accepted" && admission["mode"] == "idle", nil
}

func call(endpoint string, request map[string]any) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", endpoint, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	answer, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(answer, &reply); err != nil {
		return nil, fmt.Errorf("reply %q: %w", answer, err)
	}
	if reply["v"] != float64(1) || reply["id"] != request["id"] {
		return nil, fmt.Errorf("reply does not answer request %v: %s", request["id"], answer)
	}
	return reply, nil
}

func waitFor(ctx context.Context, limit time.Duration, ready func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	var last error
	for {
		ok, err := ready()
		if ok {
			return nil
		}
		last = err
		if err != nil && !isTransient(err) {
			return err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("timed out after %s: %w", limit, last)
			}
			return fmt.Errorf("timed out after %s", limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Socket errors while a session settles are retried; anything else is final.
func isTransient(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) || errors.Is(err, io.EOF)
}

func sameJSON(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(left, right)
}

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// tailBuffer keeps the last bytes of the session's screen output for errors.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - 8192; over > 0 {
		b.data = b.data[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) excerpt() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.Join(strings.Fields(string(stripEscapes(b.data))), " ")
	if text == "" {
		return ""
	}
	if len(text) > 600 {
		text = text[len(text)-600:]
	}
	return "; last screen output: " + text
}

func stripEscapes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == 0x1b {
			// Skip a CSI/OSC/other escape sequence.
			i++
			if i < len(data) && data[i] == '[' {
				for i+1 < len(data) && (data[i+1] < 0x40 || data[i+1] > 0x7e) {
					i++
				}
				i++
			} else if i < len(data) && data[i] == ']' {
				for i+1 < len(data) && data[i+1] != 0x07 && data[i+1] != 0x1b {
					i++
				}
			}
			continue
		}
		if c >= 0x20 || c == '\n' {
			out = append(out, c)
		}
	}
	return out
}
