package doctor

import (
	"strings"
	"testing"
)

// CheckBuild compares the build the daemon PROCESS is running against the build
// of the binary at its own path (what a restart would load). It must report skew
// only when it genuinely knows both, because both kinds of guess are harmful: a
// false alarm tells the operator to bounce a current daemon, and a false green
// hides exactly the staleness the check exists to catch.
func TestCheckBuildReportsSkewOnlyWhenBothBuildsAreIdentifiable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     BuildStatus
		wantOK     bool
		wantDetail []string
	}{
		{
			name:       "daemon not running",
			status:     BuildStatus{OnDisk: BuildInfoFromValues("dev-aaa", "aaa")},
			wantOK:     true,
			wantDetail: []string{"daemon not running"},
		},
		{
			name:       "daemon recorded no build (started by an older gitmoot)",
			status:     BuildStatus{OnDisk: BuildInfoFromValues("dev-aaa", "aaa"), DaemonRunning: true},
			wantOK:     true,
			wantDetail: []string{"daemon build unknown", "comparison skipped"},
		},
		{
			name:       "on-disk build could not be resolved",
			status:     BuildStatus{Daemon: BuildInfoFromValues("dev-aaa", "aaa"), DaemonRunning: true},
			wantOK:     true,
			wantDetail: []string{"daemon's binary path is unknown", "comparison skipped"},
		},
		{
			// An unstamped `go build` with no VCS revision is an anonymous "dev".
			// Comparing two anonymous builds proves nothing: saying "same" would
			// green-light a daemon running week-old code.
			name:       "both sides anonymous dev: unknown, not equal",
			status:     BuildStatus{Daemon: BuildInfoFromValues("dev", "unknown"), OnDisk: BuildInfoFromValues("dev", "unknown"), DaemonRunning: true},
			wantOK:     true,
			wantDetail: []string{"unknown", "comparison skipped"},
		},
		{
			// ...and the mirror image: an anonymous "dev" binary on disk must not be
			// reported as skewed against a stamped daemon, which would tell the
			// operator to restart a perfectly current daemon.
			name:       "stamped daemon vs anonymous dev on disk: unknown, not skew",
			status:     BuildStatus{Daemon: BuildInfoFromValues("dev-56ba1c7", "56ba1c74"), OnDisk: BuildInfoFromValues("dev", "unknown"), DaemonRunning: true},
			wantOK:     true,
			wantDetail: []string{"comparison skipped"},
		},
		{
			name:       "same build",
			status:     BuildStatus{Daemon: BuildInfoFromValues("v0.9.1", "aaa"), OnDisk: BuildInfoFromValues("v0.9.1", "aaa"), DaemonRunning: true},
			wantOK:     true,
			wantDetail: []string{"running the binary on disk", "v0.9.1"},
		},
		{
			name:       "versions differ — the daemon is stale",
			status:     BuildStatus{Daemon: BuildInfoFromValues("v0.9.0", "old"), OnDisk: BuildInfoFromValues("v0.9.1", "new"), DaemonRunning: true, OnDiskPath: "/usr/local/bin/gitmoot"},
			wantOK:     false,
			wantDetail: []string{"v0.9.0", "v0.9.1", "/usr/local/bin/gitmoot", "restart the daemon"},
		},
		{
			// Same version string, different code: dev builds keep Version=dev-<sha>
			// only if stamped; a VCS revision is what distinguishes them otherwise.
			name:       "same version, different commit",
			status:     BuildStatus{Daemon: BuildInfoFromValues("dev", "oldsha0000"), OnDisk: BuildInfoFromValues("dev", "newsha1111"), DaemonRunning: true},
			wantOK:     false,
			wantDetail: []string{"oldsha00", "newsha11", "restart the daemon"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := CheckBuild(tc.status)
			if check.Name != "build" {
				t.Fatalf("check name = %q", check.Name)
			}
			if check.Required {
				t.Fatal("build skew must never be a required (fatal) check")
			}
			if check.OK != tc.wantOK {
				t.Fatalf("OK = %t, want %t (detail %q)", check.OK, tc.wantOK, check.Detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("detail = %q, want it to contain %q", check.Detail, want)
				}
			}
		})
	}
}
