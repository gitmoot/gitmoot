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
	delegationWorktreeWarnCount = 10
	delegationWorktreeWarnBytes = int64(1_000_000_000)
)

type delegationWorktreeUsage struct {
	Stale          int    `json:"stale"`
	SizeBytes      int64  `json:"sizeBytes"`
	Size           string `json:"size"`
	Reclaimable    int    `json:"reclaimable"`
	Pinned         int    `json:"pinned"`
	Unproven       int    `json:"unproven"`
	RecentTerminal int    `json:"recentTerminal"`
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

// inspectDelegationWorktreeUsage accounts only for per-delegation/read-only
// worktrees under <home>/worktrees. Ordinary task worktrees are excluded. It
// combines recorded job ownership with an exact-depth directory scan so a
// crash-before-enqueue orphan is still visible as unproven, never reclaimed.
func inspectDelegationWorktreeUsage(ctx context.Context, paths config.Paths, store *db.Store, now time.Time, ttl time.Duration) (delegationWorktreeUsage, error) {
	root := filepath.Join(paths.Home, "worktrees")
	usage := delegationWorktreeUsage{Root: root}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return usage, err
	}

	owned := map[string]delegationWorktreeClass{}
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil || strings.TrimSpace(payload.WorktreePath) == "" {
			continue
		}
		if strings.TrimSpace(payload.DelegationID) == "" && !payload.ReadOnlyWorktree {
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
		if prior, exists := owned[path]; !exists || worktreeClassPriority(class) > worktreeClassPriority(prior) {
			owned[path] = class
		}
	}

	pathsOnDisk := map[string]struct{}{}
	for path := range owned {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			pathsOnDisk[path] = struct{}{}
		}
	}
	// Canonical layout: <root>/<owner--repo>/delegations/<parent>/<leg>.
	// Stop at each leg root; size accounting below walks it exactly once.
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == root || !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 4 && parts[1] == "delegations" {
			pathsOnDisk[path] = struct{}{}
			return filepath.SkipDir
		}
		return nil
	})

	ordered := make([]string, 0, len(pathsOnDisk))
	for path := range pathsOnDisk {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
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
		usage.SizeBytes += directoryLogicalSize(path)
	}
	usage.Size = formatWorktreeBytes(usage.SizeBytes)
	usage.Summary = fmt.Sprintf("%d stale worktree%s / %s under %s", usage.Stale, pluralSuffix(usage.Stale), usage.Size, usage.Root)
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

func directoryLogicalSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
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
	detail := fmt.Sprintf("%s (%d reclaimable, %d pinned by non-terminal owners, %d unproven; %d recent terminal within TTL)", usage.Summary, usage.Reclaimable, usage.Pinned, usage.Unproven, usage.RecentTerminal)
	warn := usage.Stale >= delegationWorktreeWarnCount || usage.SizeBytes >= delegationWorktreeWarnBytes
	return doctor.Check{Name: "worktrees", OK: !warn, Required: false, Detail: detail}
}
