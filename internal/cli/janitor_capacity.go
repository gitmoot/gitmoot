package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2359: default-off pressure admission. workspace-janitor owns pressure and its
// hysteresis (core.AdvancePressure) and publishes it in `janitor --format json
// status` under data.capacity; gitmoot only reads that decision and never keeps
// a second latch. While a device that new work would write to is not cleared,
// NEW heavy work waits: queued jobs stay queued (with one event each) and
// foreground dispatch refuses before it creates a worktree. Running jobs are
// never touched, and the [disk_guard] floors still apply on their own.

const janitorPressureHeldEventKind = "janitor_pressure_held"

// janitorCapacityClockSkew tolerates a producer clock slightly ahead of ours.
// A sample further in the future is not trusted.
const janitorCapacityClockSkew = 30 * time.Second

// runJanitorStatus runs the fixed argv `<executable> --format json status`: no
// shell, no interpolation. It is a variable so tests can supply producer output.
var runJanitorStatus = func(ctx context.Context, executable string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--format", "json", "status")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("janitor status timed out after %s", timeout)
		}
		return nil, fmt.Errorf("janitor status failed: %v: %s", err, firstLineTrimmed(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type janitorVolume struct {
	key           string
	device        uint64
	hasDevice     bool
	state         string
	admissionHold bool
	reason        string
}

type janitorCapacitySnapshot struct {
	sampledAt, validUntil time.Time
	volumes               []janitorVolume
}

// readJanitorCapacity runs the producer and validates its decision. Any error
// means the decision is unknown, which holds heavy work.
func readJanitorCapacity(ctx context.Context, cfg config.JanitorCapacity) (janitorCapacitySnapshot, error) {
	out, err := runJanitorStatus(ctx, cfg.Executable, cfg.Timeout)
	if err != nil {
		return janitorCapacitySnapshot{}, err
	}
	// Freshness is judged when the answer ARRIVES, not when the call started:
	// a slow producer must not turn an expired sample into an admission.
	return parseJanitorCapacity(out, cfg.PolicyHash, janitorNow())
}

// janitorNow is the consumer clock; tests replace it.
var janitorNow = time.Now

func parseJanitorCapacity(out []byte, policyHash string, now time.Time) (janitorCapacitySnapshot, error) {
	var envelope struct {
		SchemaVersion json.Number `json:"schema_version"`
		Kind          string      `json:"kind"`
		Data          struct {
			Capacity *struct {
				SampledAt  string `json:"sampled_at"`
				ValidUntil string `json:"valid_until"`
				PolicyHash string `json:"policy_hash"`
				Volumes    []struct {
					FilesystemKey string       `json:"filesystem_key"`
					Device        *json.Number `json:"device"`
					State         string       `json:"state"`
					AdmissionHold *bool        `json:"admission_hold"`
					Reason        string       `json:"reason"`
				} `json:"volumes"`
			} `json:"capacity"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor status is not valid JSON: %v", err)
	}
	// Exactly ONE document: trailing garbage or a second envelope could
	// contradict the first, so it is not trusted.
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return janitorCapacitySnapshot{}, errors.New("janitor status has data after its JSON document")
	}
	if envelope.SchemaVersion.String() != "1" || envelope.Kind != "status" {
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor status envelope schema_version=%q kind=%q is not supported (want 1, status)", envelope.SchemaVersion, envelope.Kind)
	}
	capacity := envelope.Data.Capacity
	if capacity == nil {
		return janitorCapacitySnapshot{}, errors.New("janitor publishes no capacity status (no capacity target is configured)")
	}
	if capacity.PolicyHash != policyHash {
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor capacity policy %q is not the approved policy %q", capacity.PolicyHash, policyHash)
	}
	var s janitorCapacitySnapshot
	var err error
	if s.sampledAt, err = time.Parse(time.RFC3339Nano, capacity.SampledAt); err != nil {
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor capacity sampled_at %q is not a timestamp", capacity.SampledAt)
	}
	if s.validUntil, err = time.Parse(time.RFC3339Nano, capacity.ValidUntil); err != nil {
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor capacity valid_until %q is not a timestamp", capacity.ValidUntil)
	}
	switch {
	case !s.validUntil.After(s.sampledAt):
		return janitorCapacitySnapshot{}, errors.New("janitor capacity valid_until is not after sampled_at")
	case s.sampledAt.After(now.Add(janitorCapacityClockSkew)):
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor capacity was sampled in the future (%s)", s.sampledAt.UTC().Format(time.RFC3339))
	case !now.Before(s.validUntil):
		return janitorCapacitySnapshot{}, fmt.Errorf("janitor capacity sample is stale (valid until %s)", s.validUntil.UTC().Format(time.RFC3339))
	}
	for _, raw := range capacity.Volumes {
		v := janitorVolume{key: raw.FilesystemKey, state: raw.State, reason: raw.Reason}
		// A missing admission_hold is not "false": treat the volume as holding.
		v.admissionHold = raw.AdmissionHold == nil || *raw.AdmissionHold
		if raw.Device != nil {
			if n, err := strconv.ParseUint(raw.Device.String(), 10, 64); err == nil && raw.FilesystemKey == "device:"+raw.Device.String() {
				v.device, v.hasDevice = n, true
			}
		}
		s.volumes = append(s.volumes, v)
	}
	return s, nil
}

// janitorClearedStates are the only states that may admit new heavy work, and
// only with admission_hold=false (the producer carries a critical hold through
// cleanup_needed until recovery). Every other or unknown state holds.
var janitorClearedStates = map[string]bool{"target_recovered": true, "cleanup_needed": true}

// decide returns "" when every device in devices is covered and cleared, or the
// first reason it is not. devices maps st_dev to one path that lives on it.
func (s janitorCapacitySnapshot) decide(devices map[uint64]string) string {
	keys := make([]uint64, 0, len(devices))
	for dev := range devices {
		keys = append(keys, dev)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, dev := range keys {
		var volume *janitorVolume
		for i := range s.volumes {
			if s.volumes[i].hasDevice && s.volumes[i].device == dev {
				volume = &s.volumes[i]
				break
			}
		}
		if volume == nil {
			return fmt.Sprintf("device %d (%s) is not covered by janitor capacity status", dev, devices[dev])
		}
		if volume.admissionHold || !janitorClearedStates[volume.state] {
			reason := fmt.Sprintf("janitor holds device %d (%s): state %s, admission_hold %t", dev, devices[dev], volume.state, volume.admissionHold)
			if strings.TrimSpace(volume.reason) != "" {
				reason += ": " + strings.TrimSpace(volume.reason)
			}
			return reason
		}
	}
	return ""
}

// janitorDeviceOf returns the st_dev of path, or of its nearest existing
// ancestor for a destination that does not exist yet. It never creates anything.
func janitorDeviceOf(path string) (uint64, error) {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, fmt.Errorf("no device number for %s", p)
			}
			return uint64(st.Dev), nil
		}
		if !os.IsNotExist(err) || p == filepath.Dir(p) {
			return 0, fmt.Errorf("resolve device for %s: %w", path, err)
		}
	}
}

// janitorHostPaths are the host paths new work for repo writes to: the gitmoot
// home (database, caches, artifacts, legacy worktrees), the seat scratch
// parent, the repository's registered checkout (its .git takes worktree
// metadata and objects), and a configured [worktrees] root that is local. A
// [worktrees] root on a verified network mount is not host disk; its own mount
// preflight governs it.
func janitorHostPaths(ctx context.Context, store *db.Store, paths config.Paths, repo string, knownCheckouts ...string) ([]string, error) {
	out := []string{paths.Home, seatTempParent()}
	// Checkouts the caller already resolved (a first dispatch to an unregistered
	// repo has no database row yet) count as much as registered ones.
	for _, checkout := range knownCheckouts {
		out = appendJanitorCheckout(out, checkout)
	}
	if strings.TrimSpace(repo) != "" && store != nil {
		// An unregistered repo has no checkout for new work to write to (the
		// job fails on its own); any other lookup error holds.
		record, err := store.GetRepo(ctx, repo)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("resolve checkout of %s: %w", repo, err)
		default:
			// The worker may self-heal to the primary checkout, so both count, and
			// so does each one's git storage (a linked worktree's .git lives elsewhere).
			for _, checkout := range []string{record.CheckoutPath, record.PrimaryCheckoutPath} {
				out = appendJanitorCheckout(out, checkout)
			}
		}
	}
	root, err := config.LoadWorktreeRoot(paths)
	if err != nil {
		return nil, err
	}
	if root.Configured() && !root.HasMount() {
		out = append(out, root.Root)
	}
	return out, nil
}

// appendJanitorCheckout adds a checkout, its .git (stat follows a symlinked or
// separately mounted .git to the device it really lives on), and a linked
// worktree's git dir and common dir.
func appendJanitorCheckout(out []string, checkout string) []string {
	if checkout = strings.TrimSpace(checkout); checkout == "" {
		return out
	}
	out = append(out, checkout, filepath.Join(checkout, ".git"))
	return append(out, janitorGitStorage(checkout)...)
}

// janitorGitStorage returns the git directory and common directory a checkout
// writes objects and worktree metadata to, when they differ from the checkout
// (a linked worktree's "gitdir:" file). A plain checkout returns nothing extra.
func janitorGitStorage(checkout string) []string {
	raw, err := os.ReadFile(filepath.Join(checkout, ".git"))
	if err != nil {
		return nil // a directory .git is measured directly by appendJanitorCheckout
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return nil
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(checkout, gitDir)
	}
	out := []string{gitDir}
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		dir := strings.TrimSpace(string(common))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitDir, dir)
		}
		out = append(out, filepath.Clean(dir))
	}
	return out
}

func janitorHostDevices(hostPaths []string) (map[uint64]string, error) {
	devices := make(map[uint64]string, len(hostPaths))
	for _, path := range hostPaths {
		dev, err := janitorDeviceOf(path)
		if err != nil {
			return nil, err
		}
		if _, seen := devices[dev]; !seen {
			devices[dev] = path
		}
	}
	return devices, nil
}

// janitorVerifiedMountDevice returns the device of the [worktrees] network
// mount, but only while it passes the same preflight that new worktrees on it
// require (mounted, configured source and type, root on that mount). It is a
// variable so tests can stand in for a mount.
var janitorVerifiedMountDevice = func(paths config.Paths) (uint64, bool) {
	root, err := config.LoadWorktreeRoot(paths)
	if err != nil || !root.Configured() || !root.HasMount() {
		return 0, false
	}
	placement, err := workflow.ResolveWorktreePlacement(filepath.Dir(paths.ConfigFile))
	if err != nil || placement.Preflight() != nil {
		return 0, false
	}
	var st syscall.Stat_t
	if err := syscall.Stat(root.Root, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// janitorAdmission is one consumer read: the loaded config and, when enabled,
// one producer snapshot. A config error counts as enabled, so it holds.
type janitorAdmission struct {
	cfg      config.JanitorCapacity
	cfgErr   error
	snapshot janitorCapacitySnapshot
	readErr  error
}

func loadJanitorAdmission(ctx context.Context, paths config.Paths) janitorAdmission {
	cfg, err := config.LoadJanitorCapacity(paths)
	if err != nil {
		return janitorAdmission{cfgErr: err}
	}
	a := janitorAdmission{cfg: cfg}
	if cfg.Enabled {
		a.snapshot, a.readErr = readJanitorCapacity(ctx, cfg)
	}
	return a
}

func (a janitorAdmission) enabled() bool { return a.cfgErr != nil || a.cfg.Enabled }

func (a janitorAdmission) hold(ctx context.Context, store *db.Store, paths config.Paths, repo string, knownCheckouts ...string) string {
	switch {
	case a.cfgErr != nil:
		return fmt.Sprintf("[janitor_capacity] cannot be loaded: %v", a.cfgErr)
	case !a.cfg.Enabled:
		return ""
	case a.readErr != nil:
		return a.readErr.Error()
	}
	hostPaths, err := janitorHostPaths(ctx, store, paths, repo, knownCheckouts...)
	if err != nil {
		return err.Error()
	}
	devices, err := janitorHostDevices(hostPaths)
	if err != nil {
		return err.Error()
	}
	// #2452: a checkout on the verified [worktrees] network mount writes to that
	// mount, not to host disk, and janitor never covers it. Leave its device out,
	// exactly as the worktree root itself is left out. Unverified or down: keep it.
	if dev, ok := janitorVerifiedMountDevice(paths); ok {
		delete(devices, dev)
	}
	if reason := a.snapshot.decide(devices); reason != "" {
		return reason
	}
	return a.expired()
}

// expired is the LAST check before an admission: a sample that was fresh when
// it arrived may expire while devices are resolved or later in a long pass.
func (a janitorAdmission) expired() string {
	if !janitorNow().Before(a.snapshot.validUntil) {
		return fmt.Sprintf("janitor capacity sample is stale (valid until %s)", a.snapshot.validUntil.UTC().Format(time.RFC3339))
	}
	return ""
}

// janitorPressureQueuedDispatch drops the queued jobs janitor pressure holds
// from a dispatch listing. It runs only on the forDispatch path, so diagnostic
// listings never start janitor. Jobs outside the filters pass through untouched.
func janitorPressureQueuedDispatch(ctx context.Context, worker jobWorker, jobs []db.Job, repoFilter, rootFilter string) []db.Job {
	if len(jobs) == 0 {
		return jobs
	}
	paths, err := diskGuardPaths(worker)
	admission := janitorAdmission{cfgErr: fmt.Errorf("resolve Gitmoot home: %w", err)}
	if err == nil {
		admission = loadJanitorAdmission(ctx, paths)
	}
	if !admission.enabled() {
		return jobs
	}
	reasons := map[string]string{}
	allowed := make([]db.Job, 0, len(jobs))
	now := time.Now()
	for _, job := range jobs {
		if !queuedJobMatchesRepo(job, repoFilter) || !queuedJobMatchesSession(job, rootFilter) {
			allowed = append(allowed, job)
			continue
		}
		repo := ""
		if payload, perr := daemonJobPayload(job); perr == nil {
			repo = payload.Repo
		}
		reason, cached := reasons[repo]
		if !cached {
			reason = admission.hold(ctx, worker.Store, paths, repo)
			reasons[repo] = reason
		} else if reason == "" {
			reason = admission.expired() // a cached admission must still be fresh
		}
		if reason == "" {
			allowed = append(allowed, job)
			continue
		}
		if shouldLogDiskGuard("janitor_capacity", repo, reason, now) {
			writeLine(worker.Stdout, "JANITOR PRESSURE HOLDS NEW WORK (%s): %s", repo, reason)
		}
		_ = worker.Store.AddJobEventIfAbsent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    janitorPressureHeldEventKind,
			Message: "held queued: " + reason,
		})
	}
	return allowed
}

// janitorPressureRefusal is the foreground check: it returns an error naming
// the hold, or nil when the consumer is off or the devices are cleared.
func janitorPressureRefusal(ctx context.Context, store *db.Store, paths config.Paths, repo string, knownCheckouts ...string) error {
	admission := loadJanitorAdmission(ctx, paths)
	if !admission.enabled() {
		return nil
	}
	if reason := admission.hold(ctx, store, paths, repo, knownCheckouts...); reason != "" {
		return fmt.Errorf("janitor pressure holds new heavy work: %s", reason)
	}
	return nil
}
