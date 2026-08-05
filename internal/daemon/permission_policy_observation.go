package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
)

type permissionPolicyBaselineEvent struct {
	Baseline int      `json:"baseline"`
	Current  int      `json:"current"`
	Delta    int      `json:"delta"`
	Configs  []string `json:"configs"`
}

func (d Daemon) reconcilePermissionPolicyObservation(ctx context.Context) error {
	configs, err := permissionpolicy.Inventory(ctx, d.Store)
	if err != nil {
		return fmt.Errorf("permission-policy inventory: %w", err)
	}
	keys := permissionpolicy.Keys(configs)
	baseline, initialized, err := d.Store.InitializePermissionPolicyObservationBaseline(ctx, keys)
	if err != nil {
		return fmt.Errorf("initialize permission-policy baseline: %w", err)
	}
	if initialized {
		return addPermissionPolicyObservationEvent(ctx, d.Store, permissionpolicy.BaselineEventKind, permissionPolicyBaselineEvent{
			Baseline: len(keys), Current: len(keys), Delta: 0, Configs: describePermissionPolicyKeys(keys),
		})
	}
	newConfigs := permissionpolicy.NewSinceBaseline(configs, baseline.Configs)
	if len(newConfigs) > 0 {
		return addPermissionPolicyObservationEvent(ctx, d.Store, permissionpolicy.BaselineGrowthEventKind, permissionPolicyBaselineEvent{
			Baseline: baseline.AffectedCount,
			Current:  len(keys),
			Delta:    len(keys) - baseline.AffectedCount,
			Configs:  newConfigs,
		})
	}
	if len(keys) < len(baseline.Configs) {
		lowered, err := d.Store.LowerPermissionPolicyObservationBaseline(ctx, baseline.Configs, keys)
		if err != nil {
			return fmt.Errorf("lower permission-policy baseline: %w", err)
		}
		if lowered {
			return addPermissionPolicyObservationEvent(ctx, d.Store, permissionpolicy.BaselineLoweredEventKind, permissionPolicyBaselineEvent{
				Baseline: baseline.AffectedCount, Current: len(keys), Delta: len(keys) - baseline.AffectedCount, Configs: newConfigs,
			})
		}
		return nil
	}
	return nil
}

func addPermissionPolicyObservationEvent(ctx context.Context, store *db.Store, kind string, event permissionPolicyBaselineEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = store.ClaimJobEvent(ctx, db.JobEvent{JobID: permissionpolicy.ObservationJobID, Kind: kind, Message: string(raw)})
	return err
}

func describePermissionPolicyKeys(keys []string) []string {
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, permissionpolicy.DecodeKey(key))
	}
	return result
}
