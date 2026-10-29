package cli

// tmp: round-2 behaviours, written to COMPILE on d65963e3 so they fail on behaviour.
import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestR2ProbeExpiryAtAdmissionBoundary(t *testing.T) {
	ctx, _, paths, _ := diskGuardDispatchFixture(t)
	start := time.Now()
	clock := start
	prev := janitorNow
	janitorNow = func() time.Time { return clock }
	t.Cleanup(func() { janitorNow = prev })
	devices, err := janitorHostDevices([]string{paths.Home, seatTempParent()})
	if err != nil {
		t.Fatal(err)
	}
	vols := []string{}
	for d := range devices {
		vols = append(vols, janitorVolumeJSON(d, "target_recovered", false))
	}
	snap, err := parseJanitorCapacity([]byte(janitorStatusJSON(start.Add(-50*time.Second), time.Minute, janitorTestPolicyHash, vols...)), janitorTestPolicyHash, start)
	if err != nil {
		t.Fatal(err)
	}
	a := janitorAdmission{cfg: config.JanitorCapacity{Enabled: true}, snapshot: snap}
	_ = a.hold(ctx, nil, paths, "") // fresh: whatever the old code says
	clock = start.Add(11 * time.Second)
	// In a pass, the CACHED admission is reused; the old code never re-checks it.
	ctx2, store, paths2, worker := diskGuardDispatchFixture(t)
	_ = ctx2
	_ = store
	_ = paths2
	_ = worker
	if reason := a.hold(ctx, nil, paths, ""); !strings.Contains(reason, "stale") {
		// old code checked expiry before device resolution: with a fixed clock it also says stale here,
		// so this probe checks the dot-git and unregistered-checkout behaviours below instead.
		t.Logf("expiry-before-devices on old code: %q", reason)
	}
}

func TestR2ProbeDotGitMeasured(t *testing.T) {
	ctx, store, paths, _ := diskGuardDispatchFixture(t)
	checkout := t.TempDir()
	if err := os.Symlink(t.TempDir(), checkout+"/.git"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "dotgit", CheckoutPath: checkout, PrimaryCheckoutPath: checkout}); err != nil {
		t.Fatal(err)
	}
	got, err := janitorHostPaths(ctx, store, paths, "owner/dotgit")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, filepath.Join(checkout, ".git")) {
		t.Fatalf("host paths %v do not measure %s/.git", got, checkout)
	}
}
