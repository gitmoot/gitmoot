package cockpit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// NotificationCapabilityUnavailable is the pending reason for a recipient
	// whose runtime cannot be notified directly: an unsupported runtime kind, an
	// OMP session without the Gitmoot inbox add-on, or a registration that fails
	// the ownership checks.
	NotificationCapabilityUnavailable = "runtime notification capability unavailable"
	// NotificationAwaitingNextTurn is the pending reason for Claude Code and
	// Codex recipients. They have no safe external wake; their own turn hooks
	// collect the mail.
	NotificationAwaitingNextTurn = "waiting for recipient's next turn"

	ompRegistrationVersion = 1
	ompProtocolVersion     = 1
	ompConnectTimeout      = 2 * time.Second
	// ompReplyTimeout bounds the add-on's synchronous admission reply. Admission
	// never waits for a turn, so a slow reply means the receipt is lost.
	ompReplyTimeout       = 5 * time.Second
	ompReplyLimit         = 64 << 10
	ompRegistrationLimit  = 64 << 10
	ompRegistrationSuffix = ".json"
)

// NotificationCapability reports why a Herdr agent kind cannot receive a direct
// notification. The empty string means the kind is OMP, whose Gitmoot inbox
// add-on admits notifications itself.
func NotificationCapability(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "omp":
		return ""
	case "claude", "codex":
		return NotificationAwaitingNextTurn
	default:
		return NotificationCapabilityUnavailable
	}
}

// ompTarget pins one OMP session. The add-on refuses a deliver request whose
// target is not its current runtime, session and generation.
type ompTarget struct {
	RuntimeID  string `json:"runtimeId"`
	SessionID  string `json:"sessionId"`
	Generation uint64 `json:"generation"`
}

// ompRegistration is the file the add-on writes under the OMP runtime dir.
type ompRegistration struct {
	Version    int     `json:"version"`
	RuntimeID  string  `json:"runtimeId"`
	SessionID  string  `json:"sessionId"`
	Generation *uint64 `json:"generation"`
	PID        int     `json:"pid"`
	Endpoint   string  `json:"endpoint"`
}

func (r ompRegistration) target() ompTarget {
	return ompTarget{RuntimeID: r.RuntimeID, SessionID: r.SessionID, Generation: *r.Generation}
}

type ompDeliverRequest struct {
	V       int       `json:"v"`
	ID      string    `json:"id"`
	Method  string    `json:"method"`
	Target  ompTarget `json:"target"`
	Content string    `json:"content"`
}

type ompDeliverReply struct {
	V      int    `json:"v"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

func capabilityUnavailable(detail string) *NotificationDeferred {
	if detail == "" {
		return &NotificationDeferred{Reason: NotificationCapabilityUnavailable}
	}
	return &NotificationDeferred{Reason: NotificationCapabilityUnavailable + ": " + detail}
}

// privateEntry refuses a symlink, an entry owned by another user, or one that
// group or others can access. The registry directory is the trust boundary:
// only its owner can create or swap the files the daemon reads and dials.
func privateEntry(info fs.FileInfo, want fs.FileMode) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if info.Mode().Type() != want {
		return fmt.Errorf("has file type %v", info.Mode().Type())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("owner is unavailable")
	}
	if uid := uint32(os.Geteuid()); stat.Uid != uid {
		return fmt.Errorf("is owned by uid %d, not %d", stat.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("has mode %04o; group and other access must be off", perm)
	}
	return nil
}

// findOMPRegistration returns the one registration whose pid is a foreground
// process of the recipient's pane. Every failure is a pre-write deferral.
func findOMPRegistration(dir string, pids []int) (ompRegistration, error) {
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	if err != nil {
		return ompRegistration{}, capabilityUnavailable("no OMP inbox add-on registry: " + err.Error())
	}
	if err := privateEntry(info, fs.ModeDir); err != nil {
		return ompRegistration{}, capabilityUnavailable(fmt.Sprintf("registry %s %v", dir, err))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ompRegistration{}, capabilityUnavailable("read registry: " + err.Error())
	}
	foreground := make(map[int]bool, len(pids))
	for _, pid := range pids {
		foreground[pid] = true
	}
	var matches []ompRegistration
	var refusals []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ompRegistrationSuffix) || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		registration, info, err := readOMPRegistration(path)
		if err != nil || !foreground[registration.PID] {
			// Unreadable or foreign registrations belong to other runtimes.
			continue
		}
		refusal := privateEntry(info, 0)
		if refusal == nil {
			refusal = validateOMPRegistration(dir, registration)
		}
		if refusal != nil {
			refusals = append(refusals, fmt.Sprintf("%s %v", path, refusal))
			continue
		}
		matches = append(matches, registration)
	}
	switch {
	case len(matches) == 1 && len(refusals) == 0:
		return matches[0], nil
	case len(refusals) > 0:
		return ompRegistration{}, capabilityUnavailable("refused " + strings.Join(refusals, "; "))
	case len(matches) > 1:
		return ompRegistration{}, capabilityUnavailable(fmt.Sprintf("%d registrations claim the recipient's process", len(matches)))
	default:
		return ompRegistration{}, capabilityUnavailable("")
	}
}

// readOMPRegistration never follows a symlink. An insecure file is still parsed
// so its refusal is reported only when it claims the recipient's process.
func readOMPRegistration(path string) (ompRegistration, fs.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ompRegistration{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ompRegistration{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return ompRegistration{}, nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, ompRegistrationLimit+1))
	if err != nil {
		return ompRegistration{}, nil, err
	}
	if len(data) > ompRegistrationLimit {
		return ompRegistration{}, nil, errors.New("registration is too large")
	}
	var registration ompRegistration
	if err := json.Unmarshal(data, &registration); err != nil {
		return ompRegistration{}, nil, err
	}
	return registration, info, nil
}

func validateOMPRegistration(dir string, registration ompRegistration) error {
	if registration.Version != ompRegistrationVersion {
		return fmt.Errorf("has unsupported version %d", registration.Version)
	}
	if registration.RuntimeID == "" || registration.SessionID == "" || registration.Generation == nil {
		return errors.New("does not pin a runtime, session and generation")
	}
	endpoint := filepath.Clean(registration.Endpoint)
	if !filepath.IsAbs(endpoint) || filepath.Dir(endpoint) != dir {
		return fmt.Errorf("endpoint %q is outside the registry", registration.Endpoint)
	}
	info, err := os.Lstat(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %v", err)
	}
	if err := privateEntry(info, fs.ModeSocket); err != nil {
		return fmt.Errorf("endpoint %s %v", endpoint, err)
	}
	return nil
}

func ompRequestID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// deliverOMPNotification sends one pinned deliver request. Nothing written
// means the obligation is deferred; once any byte is written, only a valid
// receipt settles it and everything else is unknown.
func deliverOMPNotification(ctx context.Context, registration ompRegistration, content string) (delivered bool, uncertain bool, err error) {
	id, err := ompRequestID()
	if err != nil {
		return false, false, &NotificationDeferred{Reason: "notification request id: " + err.Error()}
	}
	request, err := json.Marshal(ompDeliverRequest{
		V: ompProtocolVersion, ID: id, Method: "deliver",
		Target: registration.target(), Content: content,
	})
	if err != nil {
		return false, false, &NotificationDeferred{Reason: "encode notification: " + err.Error()}
	}
	request = append(request, '\n')
	dialer := net.Dialer{Timeout: ompConnectTimeout}
	conn, err := dialer.DialContext(ctx, "unix", registration.Endpoint)
	if err != nil {
		return false, false, &NotificationDeferred{Reason: "runtime notification endpoint unreachable: " + err.Error()}
	}
	defer conn.Close()
	deadline := time.Now().Add(ompReplyTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return false, false, &NotificationDeferred{Reason: "runtime notification endpoint: " + err.Error()}
	}
	written, err := conn.Write(request)
	if err != nil {
		if written == 0 {
			return false, false, &NotificationDeferred{Reason: "runtime notification endpoint refused the request: " + err.Error()}
		}
		return false, true, fmt.Errorf("notification request %s partially written: %w", id, err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, ompReplyLimit)).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) > 0) {
		return false, true, fmt.Errorf("notification receipt %s missing: %w", id, err)
	}
	var reply ompDeliverReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return false, true, fmt.Errorf("notification receipt %s unreadable: %w", id, err)
	}
	if reply.V != ompProtocolVersion || reply.ID != id {
		return false, true, fmt.Errorf("notification receipt v=%d id=%q does not answer request %s", reply.V, reply.ID, id)
	}
	switch {
	case reply.Status == "accepted" && (reply.Mode == "idle" || reply.Mode == "working"):
		return true, false, nil
	case reply.Status == "deferred" && reply.Reason != "":
		return false, false, &NotificationDeferred{Reason: reply.Reason}
	default:
		return false, true, fmt.Errorf("notification receipt %s status=%q mode=%q reason=%q is not a decision", id, reply.Status, reply.Mode, reply.Reason)
	}
}

// processInfoResult mirrors `herdr pane process-info --pane ID`.
type processInfoResult struct {
	Result struct {
		ProcessInfo struct {
			PaneID              string `json:"pane_id"`
			ForegroundProcesses []struct {
				PID int `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	} `json:"result"`
}

// paneForegroundPIDs lists the processes in the pane's foreground process group.
func (c herdrClient) paneForegroundPIDs(ctx context.Context, paneID string) ([]int, error) {
	out, err := c.run(ctx, "pane", "process-info", "--pane", paneID)
	if err != nil {
		return nil, fmt.Errorf("herdr pane process-info: %w", err)
	}
	var info processInfoResult
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return nil, fmt.Errorf("parse pane process-info: %w", err)
	}
	if info.Result.ProcessInfo.PaneID != paneID {
		return nil, fmt.Errorf("pane process-info answered for pane %q, not %q", info.Result.ProcessInfo.PaneID, paneID)
	}
	pids := make([]int, 0, len(info.Result.ProcessInfo.ForegroundProcesses))
	for _, process := range info.Result.ProcessInfo.ForegroundProcesses {
		if process.PID > 0 {
			pids = append(pids, process.PID)
		}
	}
	if len(pids) == 0 {
		return nil, errors.New("pane has no foreground process")
	}
	return pids, nil
}

// agentNotify delivers to the OMP inbox add-on of the process running in the
// recipient's pane. It never types into the pane: unsupported runtimes and
// unregistered processes stay deferred.
func (c herdrClient) agentNotify(ctx context.Context, registryDir string, target NotificationTarget, content string) (delivered bool, uncertain bool, err error) {
	if reason := NotificationCapability(target.Kind); reason != "" {
		return false, false, &NotificationDeferred{Reason: reason}
	}
	if target.PaneID == "" || registryDir == "" {
		return false, false, capabilityUnavailable("")
	}
	pids, err := c.paneForegroundPIDs(ctx, target.PaneID)
	if err != nil {
		return false, false, &NotificationDeferred{Reason: "recipient process unavailable: " + err.Error()}
	}
	registration, err := findOMPRegistration(registryDir, pids)
	if err != nil {
		return false, false, err
	}
	return deliverOMPNotification(ctx, registration, content)
}
