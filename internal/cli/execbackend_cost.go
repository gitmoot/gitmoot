package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// execBackendStoreCap converts the compute-dollar configuration into the
// store-side admission policy (#1540).
//
// It lives here rather than as a method on config because internal/db imports
// internal/config, so the reverse direction is an import cycle. The conversion
// is the only place the two shapes meet, and it FAILS CLOSED: Configured is set
// only when Admits reports true, so an absent, malformed or partially filled
// configuration reaches the store as a deny rather than as a permissive zero.
func execBackendStoreCap(cfg config.ExecBackendCostConfig) db.ExecBackendCostCap {
	if err := cfg.Validate(); err != nil {
		return db.ExecBackendCostCap{DenyReason: err.Error()}
	}
	if admits, why := cfg.Admits(); !admits {
		return db.ExecBackendCostCap{DenyReason: why}
	}
	return db.ExecBackendCostCap{
		Configured:     true,
		MaxReservedUSD: cfg.MaxReservedUSD,
		MaxConcurrent:  cfg.MaxConcurrent,
		PerAttemptUSD:  cfg.PerAttemptUSD,
	}
}

// execBackendSandboxdCap is sandboxd's admission policy: finite on-prem
// capacity, not cloud dollar spend. ceiling is the optional
// [remote_exec.sandboxd].max_concurrent (0 = none); capacity and capErr are one
// read of GET /sandboxd/capacity; template is the template the job creates.
//
// The cap is never unlimited:
//   - a report: min(ceiling, totalSlots) provider-wide, and the template's
//     totalSlots for that template. No online slot for the template is a
//     transient "capacity" refusal;
//   - 404 (a sandboxd older than the endpoint): the ceiling alone, today's
//     behaviour, or "unconfigured" without one;
//   - 401/403: "unconfigured", since a wrong key never frees;
//   - anything else: a transient "capacity" refusal.
//
// A non-nil refusal is returned before any row is written; otherwise the
// returned policy goes to the store's guarded INSERT.
func execBackendSandboxdCap(ceiling int, capacity e2b.Capacity, capErr error, template string) (db.ExecBackendCostCap, error) {
	if ceiling < 0 {
		return db.ExecBackendCostCap{DenyReason: "[remote_exec.sandboxd].max_concurrent must not be negative"}, nil
	}
	switch {
	case capErr == nil:
	case errors.Is(capErr, e2b.ErrCapacityUnsupported):
		if ceiling > 0 {
			return db.ExecBackendCostCap{Configured: true, CapacityOnly: true, MaxConcurrent: ceiling}, nil
		}
		return db.ExecBackendCostCap{DenyReason: "sandboxd does not report capacity (GET /sandboxd/capacity answered 404) and no ceiling is configured: upgrade sandboxd to a release with the capacity endpoint, or set [remote_exec.sandboxd].max_concurrent"}, nil
	case errors.Is(capErr, e2b.ErrCapacityAuth):
		return db.ExecBackendCostCap{DenyReason: "sandboxd refused the configured API key for its capacity report: check [remote_exec.sandboxd].api_key_file: " + capErr.Error()}, nil
	default:
		return db.ExecBackendCostCap{}, &db.ExecBackendCapRefusal{Clause: db.ExecBackendCapClauseCapacity,
			DenyReason: "sandboxd capacity could not be read: " + capErr.Error()}
	}
	entry, served := capacity.Template(template)
	if capacity.TotalSlots <= 0 || !served || entry.TotalSlots <= 0 {
		reason := fmt.Sprintf("sandboxd reports no online slot for template %q (cluster totalSlots=%d", template, capacity.TotalSlots)
		if served {
			reason += fmt.Sprintf(", template totalSlots=%d", entry.TotalSlots)
		} else {
			reason += ", template not served by any online worker"
		}
		reason += ")"
		if offline := capacity.OfflineWorkers(); len(offline) > 0 {
			reason += "; offline workers: " + strings.Join(offline, ", ")
		}
		return db.ExecBackendCostCap{}, &db.ExecBackendCapRefusal{Clause: db.ExecBackendCapClauseCapacity, DenyReason: reason}
	}
	limit := capacity.TotalSlots
	if ceiling > 0 && ceiling < limit {
		limit = ceiling
	}
	return db.ExecBackendCostCap{Configured: true, CapacityOnly: true, MaxConcurrent: limit, MaxConcurrentTemplate: entry.TotalSlots}, nil
}
