package doctor

import (
	"strings"
	"testing"
	"time"
)

func TestCheckDaemonLogWarnsOnlyForConfirmedStaleness(t *testing.T) {
	started := time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC)
	lastWrite := started.Add(-6 * 24 * time.Hour)
	for _, tc := range []struct {
		name       string
		status     DaemonLogStatus
		wantOK     bool
		wantDetail []string
	}{
		{
			name:   "daemon not running",
			status: DaemonLogStatus{Determined: true, Stale: true},
			wantOK: true,
		},
		{
			name:   "fresh log",
			status: DaemonLogStatus{DaemonRunning: true, Determined: true},
			wantOK: true,
		},
		{
			name:   "indeterminate state",
			status: DaemonLogStatus{DaemonRunning: true},
			wantOK: true,
		},
		{
			name: "stale log",
			status: DaemonLogStatus{
				DaemonRunning: true,
				Determined:    true,
				Stale:         true,
				LogPath:       "/tmp/gitmoot/daemon.log",
				StartedAt:     started,
				LastWrite:     lastWrite,
			},
			wantOK: false,
			wantDetail: []string{
				"/tmp/gitmoot/daemon.log",
				lastWrite.Format(time.RFC3339),
				started.Format(time.RFC3339),
				"if running under systemd",
				"journalctl --user -u gitmoot-daemon -f",
			},
		},
		{
			name: "missing log",
			status: DaemonLogStatus{
				DaemonRunning: true,
				Determined:    true,
				Stale:         true,
				Missing:       true,
				LogPath:       "/tmp/gitmoot/daemon.log",
				StartedAt:     started,
			},
			wantOK:     false,
			wantDetail: []string{"missing", started.Format(time.RFC3339), "journalctl"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := CheckDaemonLog(tc.status)
			if check.Name != "daemon log" {
				t.Fatalf("check name = %q", check.Name)
			}
			if check.Required {
				t.Fatal("daemon log staleness must never be a required check")
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
