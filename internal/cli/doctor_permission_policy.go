package cli

import (
	"context"
	"fmt"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
)

func permissionPolicyObservationDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	ctx := context.Background()
	configs, err := permissionpolicy.Inventory(ctx, store)
	if err != nil {
		return doctor.Check{Name: "permission-policy observation", Required: false, Detail: fmt.Sprintf("inventory failed: %v", err)}, true
	}
	baseline, ok, err := store.PermissionPolicyObservationBaseline(ctx)
	if err != nil {
		return doctor.Check{Name: "permission-policy observation", Required: false, Detail: fmt.Sprintf("baseline read failed: %v", err)}, true
	}
	if !ok {
		return doctor.Check{
			Name: "permission-policy observation", Required: false,
			Detail: fmt.Sprintf("current=%d baseline=not-recorded; set [daemon].permission_policy_observation_enabled=true to arm the live-store ratchet", len(configs)),
		}, true
	}
	delta := len(configs) - baseline.AffectedCount
	return doctor.Check{
		Name: "permission-policy observation", OK: delta <= 0, Required: false,
		Detail: fmt.Sprintf("current=%d baseline=%d delta=%+d", len(configs), baseline.AffectedCount, delta),
	}, true
}
