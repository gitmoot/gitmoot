// Package agyaddon manages the Antigravity CLI (`agy`) inbox notification
// plugin and private runtime registry (<gitmoot home>/run/agy).
//
// Antigravity CLI lifecycle hooks (`SessionStart`, `PreInvocation`, `Stop`,
// `PreToolUse`, `PostToolUse`) inherit `ANTIGRAVITY_CONVERSATION_ID`, but only
// `run_command` subprocesses inherit `ANTIGRAVITY_LS_ADDRESS` and
// `ANTIGRAVITY_CSRF_TOKEN`. Gitmoot therefore combines:
//  1. Opportunistic registration (`RegisterFromEnv`) after a successful
//     `gitmoot message inbox|show|send|reply` inside an interactive top-level
//     `agy` `run_command` shell (or explicitly via `gitmoot plugin agy-register`),
//     writing `<registry>/<pid>.json` (mode 0600 inside a mode 0700 dir).
//  2. A native Antigravity plugin (`~/.gemini/config/plugins/gitmoot/`) whose
//     `SessionStart` and `PreInvocation` hooks keep `<registry>/<pid>.conv.json`
//     pinned to the active interactive top-level conversation across `/new`
//     and `/resume` switches.
package agyaddon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// RegistrationVersion is the schema version written to <pid>.json and <pid>.conv.json.
	RegistrationVersion = 1

	// RegistrationSuffix and ConvPinSuffix identify the two record types in AgyRuntimeDir.
	RegistrationSuffix = ".json"
	ConvPinSuffix      = ".conv.json"

	// MaxFileBytes bounds registration and conversation-pin reads.
	MaxFileBytes = 64 << 10

	// Environment variables exported by Antigravity CLI to run_command subprocesses.
	EnvLSAddress      = "ANTIGRAVITY_LS_ADDRESS"
	EnvCSRFToken      = "ANTIGRAVITY_CSRF_TOKEN"
	EnvConversationID = "ANTIGRAVITY_CONVERSATION_ID"
	EnvHerdrPaneID    = "HERDR_PANE_ID"

	// InteractiveMode is the interactionMode reported for interactive TUI sessions.
	InteractiveMode = "INTERACTION_MODE_INTERACTIVE"

	metadataProbeTimeout = 3 * time.Second
)

// Registration records one live interactive top-level `agy` process in AgyRuntimeDir.
type Registration struct {
	Version        int    `json:"version"`
	PID            int    `json:"pid"`
	StartTimeTicks uint64 `json:"startTimeTicks"`
	LSAddress      string `json:"lsAddress"`
	CSRFToken      string `json:"csrfToken"`
	ConversationID string `json:"conversationId"`
	HerdrPaneID    string `json:"herdrPaneId,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

// ConversationPin records the most recent verified interactive top-level
// conversationId observed by an `agy` lifecycle hook for a registered `agy` process.
type ConversationPin struct {
	Version        int    `json:"version"`
	PID            int    `json:"pid"`
	StartTimeTicks uint64 `json:"startTimeTicks"`
	ConversationID string `json:"conversationId"`
	Event          string `json:"event,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
}

// ValidateLoopbackAddress verifies that addr is host:port pointing strictly at
// localhost, 127.0.0.1, or ::1 with a valid non-zero port.
func ValidateLoopbackAddress(addr string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("invalid lsAddress %q: %w", addr, err)
	}
	lowerHost := strings.ToLower(strings.TrimSpace(host))
	if lowerHost != "localhost" && lowerHost != "127.0.0.1" && lowerHost != "::1" {
		return "", 0, fmt.Errorf("lsAddress %q is not loopback", addr)
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return "", 0, fmt.Errorf("invalid port in lsAddress %q", addr)
	}
	return lowerHost, uint16(portNum), nil
}

// RegistrationPath returns the canonical path for pid's registration file.
func RegistrationPath(dir string, pid int) string {
	return filepath.Join(filepath.Clean(dir), fmt.Sprintf("%d%s", pid, RegistrationSuffix))
}

// ConvPinPath returns the canonical path for pid's hook-tracked conversation pin.
func ConvPinPath(dir string, pid int) string {
	return filepath.Join(filepath.Clean(dir), fmt.Sprintf("%d%s", pid, ConvPinSuffix))
}

func registryLockPath(dir string) string {
	return filepath.Join(filepath.Clean(dir), ".registry.lock")
}

// EnsureRegistryDir creates dir with mode 0700 and verifies it is a private
// non-symlink directory owned by the current effective UID.
func EnsureRegistryDir(dir string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("agy registry directory %q is not absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("agy registry %s is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("agy registry %s is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
		if err != nil {
			return err
		}
	}
	return CheckPrivateEntry(info, fs.ModeDir)
}

// ReadRegistration reads and parses a registration JSON file without following symlinks.
func ReadRegistration(path string) (Registration, fs.FileInfo, error) {
	data, info, err := readBoundedFile(path)
	if err != nil {
		return Registration{}, nil, err
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registration{}, nil, err
	}
	return reg, info, nil
}

// ReadConvPin reads and parses a conversation-pin JSON file without following symlinks.
func ReadConvPin(path string) (ConversationPin, fs.FileInfo, error) {
	data, info, err := readBoundedFile(path)
	if err != nil {
		return ConversationPin{}, nil, err
	}
	var pin ConversationPin
	if err := json.Unmarshal(data, &pin); err != nil {
		return ConversationPin{}, nil, err
	}
	return pin, info, nil
}

func readBoundedFile(path string) ([]byte, fs.FileInfo, error) {
	file, info, err := openRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, nil, errors.New("file is too large")
	}
	return data, info, nil
}

// PruneDeadRegistrations removes leftover <pid>.json and <pid>.conv.json files
// in dir whose owning process no longer exists or has a different kernel start
// time. Best-effort: symlinks and untrusted entries are skipped.
func PruneDeadRegistrations(dir string) {
	PruneDeadRegistrationsWith(dir, ProcessAlive, ProcessStartTime)
}

// PruneDeadRegistrationsWith is the testable form of PruneDeadRegistrations.
func PruneDeadRegistrationsWith(dir string, alive func(int) bool, startTime func(int) (uint64, error)) {
	dir = filepath.Clean(dir)
	info, err := os.Lstat(dir)
	if err != nil || CheckPrivateEntry(info, fs.ModeDir) != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		var (
			pidStr string
			isConv bool
		)
		switch {
		case strings.HasSuffix(name, ConvPinSuffix):
			pidStr = strings.TrimSuffix(name, ConvPinSuffix)
			isConv = true
		case strings.HasSuffix(name, RegistrationSuffix):
			pidStr = strings.TrimSuffix(name, RegistrationSuffix)
		default:
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		path := filepath.Join(dir, name)
		fi, err := os.Lstat(path)
		if err != nil || CheckPrivateEntry(fi, 0) != nil {
			continue
		}
		if !alive(pid) {
			_ = os.Remove(path)
			continue
		}
		liveStart, err := startTime(pid)
		if err != nil || liveStart == 0 {
			continue
		}
		var recordedStart uint64
		if isConv {
			if pin, _, err := ReadConvPin(path); err == nil {
				recordedStart = pin.StartTimeTicks
			}
		} else {
			if reg, _, err := ReadRegistration(path); err == nil {
				recordedStart = reg.StartTimeTicks
			}
		}
		if recordedStart != 0 && recordedStart != liveStart {
			_ = os.Remove(path)
		}
	}
}

// MetadataVerifier returns the interactionMode and nestingDepth of conversationID
// as reported by the local Antigravity language server.
type MetadataVerifier func(ctx context.Context, lsAddress, csrfToken, conversationID string) (interactionMode string, nestingDepth int, err error)

// PortOwnerVerifier verifies that pid owns lsAddress and returns the concrete
// numeric loopback address (`127.0.0.1:<port>` or `[::1]:<port>`) bound by pid.
type PortOwnerVerifier func(pid int, lsAddress string) (string, error)

// RegisterOptions configures RegisterFromEnv. Nil function fields use defaults.
type RegisterOptions struct {
	RegistryDir    string
	AgyBin         string
	Getenv         func(string) string
	FindAncestor   func() (pid int, startTime uint64, err error)
	VerifyPort     PortOwnerVerifier
	VerifyMetadata MetadataVerifier
	Now            func() time.Time
	PruneDead      bool
}

// RegisterFromEnv registers the current interactive top-level `agy` session
// from ANTIGRAVITY_* environment variables into RegistryDir. It returns
// (true, nil) when a registration was written or updated, and (false, nil)
// when no update was needed (e.g. already up to date, or not inside agy).
func RegisterFromEnv(ctx context.Context, opts RegisterOptions) (bool, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	lsAddress := strings.TrimSpace(getenv(EnvLSAddress))
	csrfToken := strings.TrimSpace(getenv(EnvCSRFToken))
	convID := strings.TrimSpace(getenv(EnvConversationID))
	if lsAddress == "" || csrfToken == "" || convID == "" {
		return false, nil
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	// Capture observation time before metadata verification so a slow command
	// completing after a newer pane switch cannot supersede the newer pin.
	observedAt := nowFn().UTC()

	if _, _, err := ValidateLoopbackAddress(lsAddress); err != nil {
		return false, err
	}
	findAncestor := opts.FindAncestor
	if findAncestor == nil {
		findAncestor = func() (int, uint64, error) {
			return FindAncestorAgyPID(os.Getppid())
		}
	}
	pid, startTime, err := findAncestor()
	if err != nil {
		return false, err
	}
	if startTime == 0 {
		return false, errors.New("agy process start time unavailable")
	}

	verifyPort := opts.VerifyPort
	if verifyPort == nil {
		verifyPort = VerifyPIDOwnsLoopbackPort
	}
	concreteAddr, err := verifyPort(pid, lsAddress)
	if err != nil {
		return false, err
	}
	if _, _, err := ValidateLoopbackAddress(concreteAddr); err != nil {
		return false, err
	}

	regPath := RegistrationPath(opts.RegistryDir, pid)
	paneID := strings.TrimSpace(getenv(EnvHerdrPaneID))
	if existing, info, err := ReadRegistration(regPath); err == nil && CheckPrivateEntry(info, 0) == nil {
		if existing.Version == RegistrationVersion &&
			existing.PID == pid &&
			existing.StartTimeTicks == startTime &&
			existing.LSAddress == concreteAddr &&
			existing.CSRFToken == csrfToken &&
			existing.ConversationID == convID &&
			existing.HerdrPaneID == paneID {
			return false, nil
		}
	}

	verifyMeta := opts.VerifyMetadata
	if verifyMeta == nil {
		agyBin := opts.AgyBin
		if agyBin == "" {
			agyBin = "agy"
		}
		verifyMeta = func(callCtx context.Context, addr, token, id string) (string, int, error) {
			return DefaultVerifyMetadata(callCtx, agyBin, addr, token, id)
		}
	}
	mode, depth, err := verifyMeta(ctx, concreteAddr, csrfToken, convID)
	if err != nil {
		return false, err
	}
	if mode != InteractiveMode {
		return false, fmt.Errorf("conversation %s is %q, not interactive", convID, mode)
	}
	if depth != 0 {
		return false, fmt.Errorf("conversation %s has nestingDepth %d (subagent)", convID, depth)
	}

	if err := EnsureRegistryDir(opts.RegistryDir); err != nil {
		return false, err
	}
	if opts.PruneDead {
		PruneDeadRegistrations(opts.RegistryDir)
	}
	wrote := false
	err = withLockFile(registryLockPath(opts.RegistryDir), func() error {
		effectiveConv := convID
		effectiveTime := observedAt

		// If a newer registration for this same live process already committed,
		// preserve its conversation and timestamp; refresh only endpoint/token/pane if changed.
		if existing, info, err := ReadRegistration(regPath); err == nil && CheckPrivateEntry(info, 0) == nil {
			if existing.Version == RegistrationVersion && existing.PID == pid && existing.StartTimeTicks == startTime {
				if existingTime, err := time.Parse(time.RFC3339Nano, existing.UpdatedAt); err == nil && !observedAt.After(existingTime) {
					if existing.LSAddress == concreteAddr && existing.CSRFToken == csrfToken && existing.HerdrPaneID == paneID {
						return nil
					}
					effectiveConv = existing.ConversationID
					effectiveTime = existingTime
				}
			}
		}

		// Never let an older command registration supersede a newer hook pin
		// that committed while this metadata probe was in flight.
		if existingPin, pinInfo, err := ReadConvPin(ConvPinPath(opts.RegistryDir, pid)); err == nil && CheckPrivateEntry(pinInfo, 0) == nil {
			if existingPin.Version == RegistrationVersion &&
				existingPin.PID == pid &&
				existingPin.StartTimeTicks == startTime &&
				strings.TrimSpace(existingPin.ConversationID) != "" {
				if pinTime, err := time.Parse(time.RFC3339Nano, existingPin.UpdatedAt); err == nil && !effectiveTime.After(pinTime) {
					effectiveConv = strings.TrimSpace(existingPin.ConversationID)
				}
			}
		}

		record := Registration{
			Version:        RegistrationVersion,
			PID:            pid,
			StartTimeTicks: startTime,
			LSAddress:      concreteAddr,
			CSRFToken:      csrfToken,
			ConversationID: effectiveConv,
			HerdrPaneID:    paneID,
			UpdatedAt:      effectiveTime.Format(time.RFC3339Nano),
		}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := writeFileAtomic(regPath, data, 0o600); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// HookConversationOptions configures RecordHookConversation.
type HookConversationOptions struct {
	RegistryDir    string
	PID            int
	StartTimeTicks uint64
	ConversationID string
	Event          string
	AgyBin         string
	VerifyPort     PortOwnerVerifier
	VerifyMetadata MetadataVerifier
	Now            func() time.Time
}

// RecordHookConversation updates <registry>/<pid>.conv.json when pid already
// has a trusted registration matching startTime AND conversationID is verified
// as an interactive top-level conversation (nestingDepth == 0) on pid's owned
// language server. The event timestamp is captured before metadata verification
// and serialized under a per-PID lock so an older hook finishing late never
// overwrites a newer conversation pin or registration.
func RecordHookConversation(ctx context.Context, opts HookConversationOptions) (bool, error) {
	conversationID := strings.TrimSpace(opts.ConversationID)
	if opts.RegistryDir == "" || opts.PID <= 0 || opts.StartTimeTicks == 0 || conversationID == "" {
		return false, nil
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	observedAt := nowFn().UTC()

	regPath := RegistrationPath(opts.RegistryDir, opts.PID)
	reg, info, err := ReadRegistration(regPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := CheckPrivateEntry(info, 0); err != nil {
		return false, err
	}
	if reg.Version != RegistrationVersion || reg.PID != opts.PID || reg.StartTimeTicks != opts.StartTimeTicks {
		return false, nil
	}

	verifyPort := opts.VerifyPort
	if verifyPort == nil {
		verifyPort = VerifyPIDOwnsLoopbackPort
	}
	concreteAddr, err := verifyPort(opts.PID, reg.LSAddress)
	if err != nil {
		return false, err
	}

	verifyMeta := opts.VerifyMetadata
	if verifyMeta == nil {
		agyBin := opts.AgyBin
		if agyBin == "" {
			agyBin = "agy"
		}
		verifyMeta = func(callCtx context.Context, addr, token, id string) (string, int, error) {
			return DefaultVerifyMetadata(callCtx, agyBin, addr, token, id)
		}
	}
	mode, depth, err := verifyMeta(ctx, concreteAddr, reg.CSRFToken, conversationID)
	if err != nil {
		return false, err
	}
	if mode != InteractiveMode || depth != 0 {
		// Subagents (nestingDepth > 0) and non-interactive runs fire hooks under
		// the parent agy PID; never let them retarget the pane's active conversation.
		return false, nil
	}

	if err := EnsureRegistryDir(opts.RegistryDir); err != nil {
		return false, err
	}
	wrote := false
	err = withLockFile(registryLockPath(opts.RegistryDir), func() error {
		// Re-check registration and existing pin under lock to reject out-of-order
		// writes from a hook whose metadata probe finished after a newer event.
		latestReg, regInfo, err := ReadRegistration(regPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := CheckPrivateEntry(regInfo, 0); err != nil {
			return err
		}
		if latestReg.Version != RegistrationVersion || latestReg.PID != opts.PID || latestReg.StartTimeTicks != opts.StartTimeTicks {
			return nil
		}
		if regTime, err := time.Parse(time.RFC3339Nano, latestReg.UpdatedAt); err == nil && !observedAt.After(regTime) {
			return nil
		}
		pinPath := ConvPinPath(opts.RegistryDir, opts.PID)
		if existingPin, pinInfo, err := ReadConvPin(pinPath); err == nil && CheckPrivateEntry(pinInfo, 0) == nil {
			if existingPin.Version == RegistrationVersion &&
				existingPin.PID == opts.PID &&
				existingPin.StartTimeTicks == opts.StartTimeTicks {
				if existingTime, err := time.Parse(time.RFC3339Nano, existingPin.UpdatedAt); err == nil && !observedAt.After(existingTime) {
					return nil
				}
			}
		}
		pin := ConversationPin{
			Version:        RegistrationVersion,
			PID:            opts.PID,
			StartTimeTicks: opts.StartTimeTicks,
			ConversationID: conversationID,
			Event:          strings.TrimSpace(opts.Event),
			UpdatedAt:      observedAt.Format(time.RFC3339Nano),
		}
		data, err := json.Marshal(pin)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := writeFileAtomic(pinPath, data, 0o600); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return wrote, nil
}

// DefaultVerifyMetadata runs `<agyBin> agentapi get-conversation-metadata <id>`
// passing ANTIGRAVITY_LS_ADDRESS and ANTIGRAVITY_CSRF_TOKEN strictly via env.
func DefaultVerifyMetadata(ctx context.Context, agyBin, lsAddress, csrfToken, conversationID string) (string, int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, metadataProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, agyBin, "agentapi", "get-conversation-metadata", conversationID)
	cmd.Env = filteredEnvWithAgy(os.Environ(), lsAddress, csrfToken)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("agentapi get-conversation-metadata: %w", err)
	}
	var parsed struct {
		Error    string `json:"error"`
		Response struct {
			ConversationMetadata struct {
				Metadata struct {
					InteractionMode string `json:"interactionMode"`
					NestingDepth    int    `json:"nestingDepth"`
				} `json:"metadata"`
			} `json:"conversationMetadata"`
		} `json:"response"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return "", 0, fmt.Errorf("parse conversation metadata: %w", err)
	}
	if parsed.Error != "" {
		return "", 0, fmt.Errorf("conversation metadata error: %s", parsed.Error)
	}
	meta := parsed.Response.ConversationMetadata.Metadata
	return meta.InteractionMode, meta.NestingDepth, nil
}

func filteredEnvWithAgy(base []string, lsAddress, csrfToken string) []string {
	out := make([]string, 0, len(base)+2)
	for _, entry := range base {
		if strings.HasPrefix(entry, EnvLSAddress+"=") || strings.HasPrefix(entry, EnvCSRFToken+"=") {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, EnvLSAddress+"="+lsAddress, EnvCSRFToken+"="+csrfToken)
	return out
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	committed = true
	return nil
}
