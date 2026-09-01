package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	delegationWorktreeWarnCount           = 10
	delegationWorktreeWarnBytes           = int64(1_000_000_000)
	delegationWorktreeTrackedPathLimit    = 4096
	delegationWorktreeDiscoveryEntryLimit = 4096
	delegationWorktreeSizeEntryLimit      = 4096
)

type delegationWorktreeUsage struct {
	Stale          int    `json:"stale"`
	SizeBytes      int64  `json:"sizeBytes"`
	Size           string `json:"size"`
	Reclaimable    int    `json:"reclaimable"`
	Pinned         int    `json:"pinned"`
	Unproven       int    `json:"unproven"`
	RecentTerminal int    `json:"recentTerminal"`
	Quarantined    int    `json:"quarantined"`
	Truncated      bool   `json:"truncated"`
	Root           string `json:"root"`
	Summary        string `json:"summary"`
}

type delegationWorktreeClass int

const (
	worktreeRecentTerminal delegationWorktreeClass = iota
	worktreeReclaimable
	worktreePinned
	worktreeUnproven
)

// inspectDelegationWorktreeUsage accounts for per-delegation/read-only worktrees
// under <home>/worktrees, plus the interrupted-removal quarantine siblings of fix
// clones — a quarantine is unowned garbage on disk that no other surface reports.
// Ordinary task worktrees are excluded. It combines recorded job ownership with an
// exact-depth directory scan so a crash-before-enqueue orphan is still visible as
// unproven, never reclaimed.
func inspectDelegationWorktreeUsage(ctx context.Context, paths config.Paths, store *db.Store, now time.Time, ttl time.Duration) (delegationWorktreeUsage, error) {
	root := filepath.Join(paths.Home, "worktrees")
	usage := delegationWorktreeUsage{Root: root}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return usage, err
	}
	usage.Quarantined, err = store.CountQuarantinedCleanupObligations(ctx)
	if err != nil {
		return usage, err
	}

	owned := map[string]delegationWorktreeClass{}
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil || strings.TrimSpace(payload.WorktreePath) == "" {
			continue
		}
		fixClone := payload.FixWorktree
		if strings.TrimSpace(payload.DelegationID) == "" && !payload.ReadOnlyWorktree && !fixClone {
			continue // ordinary task/shared-checkout payload
		}
		path, ok := worktreePathUnderRoot(root, payload.WorktreePath)
		if !ok {
			continue
		}
		class := worktreePinned
		if workflow.IsFinalJobState(job.State) {
			stamp := parseJobTimeMillis(job.UpdatedAt)
			if stamp == 0 {
				stamp = parseJobTimeMillis(job.CreatedAt)
			}
			switch {
			case stamp == 0:
				class = worktreeUnproven
			case ttl > 0 && !time.UnixMilli(stamp).After(now.Add(-ttl)):
				class = worktreeReclaimable
			default:
				class = worktreeRecentTerminal
			}
		}
		// A path referenced by multiple rows is pinned if ANY owner is resumable;
		// safety wins over an older terminal record for the same deterministic path.
		if prior, exists := owned[path]; exists {
			if worktreeClassPriority(class) > worktreeClassPriority(prior) {
				owned[path] = class
			}
			continue
		}
		if len(owned) >= delegationWorktreeTrackedPathLimit {
			usage.Truncated = true
			continue
		}
		owned[path] = class
	}

	pathsOnDisk := map[string]struct{}{}
	unprovenEntries := map[string]struct{}{}
	for path := range owned {
		if info, err := os.Lstat(path); err == nil {
			if info.IsDir() {
				pathsOnDisk[path] = struct{}{}
			} else {
				unprovenEntries[path] = struct{}{}
			}
		}
	}

	// Canonical directory roots:
	//   <root>/<owner--repo>/delegations/<parent>/<leg>
	//   <root>/<owner--repo>/fixes/<managed-or-set-aside-clone>
	//
	// The FIXES arm is structural rather than job-driven: interrupted pre-enqueue
	// allocation has no job row by definition. A single global entry budget bounds
	// doctor and /api/health even when the host tree is attacker-controlled.
	discovered := 0
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, entryErr error) error {
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if entryErr != nil {
			usage.Truncated = true
			return nil
		}
		if path == root {
			return nil
		}
		discovered++
		if discovered > delegationWorktreeDiscoveryEntryLimit {
			usage.Truncated = true
			return fs.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			usage.Truncated = true
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		isDelegationRoot := len(parts) == 4 && parts[1] == "delegations"
		isFixRoot := len(parts) == 3 && parts[1] == "fixes"
		if !isDelegationRoot && !isFixRoot {
			return nil
		}
		if entry.IsDir() {
			if len(pathsOnDisk) >= delegationWorktreeTrackedPathLimit {
				usage.Truncated = true
				return fs.SkipAll
			}
			pathsOnDisk[path] = struct{}{}
			return filepath.SkipDir
		}
		unprovenEntries[path] = struct{}{}
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		usage.Truncated = true
	}
	if ctx.Err() != nil {
		return usage, ctx.Err()
	}
	usage.Stale += len(unprovenEntries)
	usage.Unproven += len(unprovenEntries)

	ordered := make([]string, 0, len(pathsOnDisk))
	for path := range pathsOnDisk {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	sizeEntriesRemaining := delegationWorktreeSizeEntryLimit
	for _, path := range ordered {
		class, ok := owned[path]
		if !ok {
			class = worktreeUnproven
		}
		if class == worktreeRecentTerminal {
			usage.RecentTerminal++
			continue
		}
		usage.Stale++
		switch class {
		case worktreeReclaimable:
			usage.Reclaimable++
		case worktreePinned:
			usage.Pinned++
		case worktreeUnproven:
			usage.Unproven++
		}
		size, truncated := directoryLogicalSize(ctx, path, &sizeEntriesRemaining)
		usage.SizeBytes += size
		usage.Truncated = usage.Truncated || truncated
	}
	usage.Size = formatWorktreeBytes(usage.SizeBytes)
	usage.Summary = fmt.Sprintf("%d stale worktree%s / %s under %s", usage.Stale, pluralSuffix(usage.Stale), usage.Size, usage.Root)
	if usage.Truncated {
		usage.Summary += " (bounded scan truncated; counts and bytes are lower bounds)"
	}
	return usage, nil
}

func worktreeClassPriority(class delegationWorktreeClass) int {
	switch class {
	case worktreePinned:
		return 4
	case worktreeUnproven:
		return 3
	case worktreeRecentTerminal:
		return 2
	case worktreeReclaimable:
		return 1
	default:
		return 0
	}
}

func worktreePathUnderRoot(root, candidate string) (string, bool) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return "", false
	}
	path, err := filepath.Abs(strings.TrimSpace(candidate))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean(path), true
}

func directoryLogicalSize(ctx context.Context, root string, remaining *int) (int64, bool) {
	var total int64
	truncated := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			truncated = true
			return fs.SkipAll
		}
		if walkErr != nil {
			truncated = true
			return nil
		}
		if *remaining <= 0 {
			truncated = true
			return fs.SkipAll
		}
		*remaining--
		if entry.IsDir() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			total += info.Size()
		} else {
			truncated = true
		}
		return nil
	})
	if err != nil {
		truncated = true
	}
	return total, truncated
}

func formatWorktreeBytes(size int64) string {
	const (
		kb = int64(1_000)
		mb = int64(1_000_000)
		gb = int64(1_000_000_000)
	)
	switch {
	case size >= gb:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(gb))
	case size >= mb:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(mb))
	case size >= kb:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(kb))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func delegationWorktreeDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	ttl, err := config.LoadDelegationWorktreeTTL(paths)
	if err != nil {
		return doctor.Check{Name: "worktrees", Required: false, Detail: fmt.Sprintf("cannot read delegation worktree TTL: %v", err)}, true
	}
	usage, err := inspectDelegationWorktreeUsage(context.Background(), paths, store, time.Now().UTC(), ttl)
	if err != nil {
		return doctor.Check{}, false
	}
	return buildDelegationWorktreeDoctorCheck(usage), true
}

func buildDelegationWorktreeDoctorCheck(usage delegationWorktreeUsage) doctor.Check {
	detail := fmt.Sprintf("%s (%d reclaimable, %d pinned by non-terminal owners, %d unproven; %d recent terminal within TTL; %d cleanup quarantined)", usage.Summary, usage.Reclaimable, usage.Pinned, usage.Unproven, usage.RecentTerminal, usage.Quarantined)
	warn := usage.Truncated || usage.Stale >= delegationWorktreeWarnCount || usage.SizeBytes >= delegationWorktreeWarnBytes || usage.Quarantined > 0
	return doctor.Check{Name: "worktrees", OK: !warn, Required: false, Detail: detail}
}
