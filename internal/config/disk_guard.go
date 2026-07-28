package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultDiskGuardMinFreeBytes leaves enough absolute headroom for SQLite,
	// logs, and ordinary checkout growth on small filesystems.
	DefaultDiskGuardMinFreeBytes uint64 = 2 << 30
	// DefaultDiskGuardMinFreePercent scales the floor on larger filesystems where
	// a fixed byte reserve would be too small relative to agent cache growth.
	DefaultDiskGuardMinFreePercent = 5.0
)

// DiskGuardPolicy controls the default-on dispatch guard for the filesystem
// holding the Gitmoot home and its worktrees.
type DiskGuardPolicy struct {
	Enabled        bool
	MinFreeBytes   uint64
	MinFreePercent float64
}

func DefaultDiskGuardPolicy() DiskGuardPolicy {
	return DiskGuardPolicy{
		Enabled:        true,
		MinFreeBytes:   DefaultDiskGuardMinFreeBytes,
		MinFreePercent: DefaultDiskGuardMinFreePercent,
	}
}

// LoadDiskGuardPolicy reads the independent [disk_guard] section. Missing
// configuration keeps the guard enabled with conservative defaults.
func LoadDiskGuardPolicy(paths Paths) (DiskGuardPolicy, error) {
	policy := DefaultDiskGuardPolicy()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return policy, nil
		}
		return DiskGuardPolicy{}, fmt.Errorf("read disk guard config: %w", err)
	}

	current := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasSuffix(line, "]") {
				current = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			} else {
				current = ""
			}
			continue
		}
		if current != "disk_guard" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "enabled":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return DiskGuardPolicy{}, fmt.Errorf("parse [disk_guard].enabled: %w", err)
			}
			policy.Enabled = parsed
		case "min_free_bytes":
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return DiskGuardPolicy{}, fmt.Errorf("parse [disk_guard].min_free_bytes: %w", err)
			}
			policy.MinFreeBytes = parsed
		case "min_free_percent":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return DiskGuardPolicy{}, fmt.Errorf("parse [disk_guard].min_free_percent: %w", err)
			}
			policy.MinFreePercent = parsed
		}
	}
	if err := validateDiskGuardPolicy(policy); err != nil {
		return DiskGuardPolicy{}, err
	}
	return policy, nil
}

func validateDiskGuardPolicy(policy DiskGuardPolicy) error {
	if math.IsNaN(policy.MinFreePercent) || math.IsInf(policy.MinFreePercent, 0) ||
		policy.MinFreePercent < 0 || policy.MinFreePercent > 100 {
		return fmt.Errorf("disk_guard.min_free_percent must be between 0 and 100")
	}
	if policy.Enabled && policy.MinFreeBytes == 0 && policy.MinFreePercent == 0 {
		return fmt.Errorf("disk_guard requires min_free_bytes or min_free_percent when enabled")
	}
	return nil
}
