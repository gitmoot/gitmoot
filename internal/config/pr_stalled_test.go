package config

import (
	"testing"
	"time"
)

func TestLoadPRStalledPolicyRefusesInvalidValues(t *testing.T) {
	for _, body := range []string{
		"[pr_stalled]\ninterval = \"0s\"\n",
		"[pr_stalled]\nci_red_after = \"-1h\"\n",
		"[pr_stalled]\nenabled = yes\n",
		"[pr_stalled]\nrepos = [\"not a repo\"]\n",
		"[pr_stalled]\ndraft_after = \"1h\"\n",
		"[pr_stalled\nenabled = true\n",
	} {
		if _, err := LoadPRStalledPolicy(writeGateConfig(t, body)); err == nil {
			t.Fatalf("LoadPRStalledPolicy accepted %q", body)
		}
	}
	policy, err := LoadPRStalledPolicy(writeGateConfig(t, "[pr_stalled]\nenabled = true\ninterval = \"45m\"\nskip_repos = [\"gitmoot/*\"]\n[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil || !policy.Enabled || policy.Interval != 45*time.Minute || policy.Covers("gitmoot/gitmoot") || !policy.Covers("other/repo") {
		t.Fatalf("policy = %+v err=%v", policy, err)
	}
}
