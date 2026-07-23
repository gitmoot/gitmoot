package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const DefaultWorkflowAutoSettleAfter = 24 * time.Hour

// WorkflowLifecyclePolicy controls conservative workflow lifecycle maintenance.
type WorkflowLifecyclePolicy struct {
	AutoSettleAfter time.Duration
}

// LoadWorkflowLifecycle resolves the independent
// [workflow].auto_settle_after setting. Omitted or empty uses 24 hours, zero
// disables auto-settle, and every other value must be a non-negative Go
// duration.
func LoadWorkflowLifecycle(paths Paths) (WorkflowLifecyclePolicy, error) {
	policy := WorkflowLifecyclePolicy{AutoSettleAfter: DefaultWorkflowAutoSettleAfter}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return policy, nil
		}
		return WorkflowLifecyclePolicy{}, err
	}
	current := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if current != "workflow" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "auto_settle_after" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = parseConfigString(value)
			if err != nil {
				return WorkflowLifecyclePolicy{}, fmt.Errorf("parse [workflow].auto_settle_after: %w", err)
			}
		}
		value = strings.TrimSpace(value)
		if value == "" {
			policy.AutoSettleAfter = DefaultWorkflowAutoSettleAfter
			continue
		}
		after, err := time.ParseDuration(value)
		if err != nil || after < 0 {
			if err == nil {
				err = fmt.Errorf("duration must not be negative")
			}
			return WorkflowLifecyclePolicy{}, fmt.Errorf("invalid [workflow].auto_settle_after %q: %w", value, err)
		}
		policy.AutoSettleAfter = after
	}
	return policy, nil
}
