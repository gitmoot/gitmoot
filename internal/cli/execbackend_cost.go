package cli

import (
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
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
