package db

import (
	"context"
	"testing"
	"time"
)

func TestJobRuntimeLockLiveness(t *testing.T) {
	now := time.Date(2026, 6, 30, 6, 0, 0, 0, time.UTC)
	const host = "host-a"
	livePID := func(int64) bool { return true }
	deadPID := func(int64) bool { return false }

	cases := []struct {
		name       string
		acquire    bool
		key        string
		pid        int64
		hostname   string
		expiresIn  time.Duration
		pidAlive   func(int64) bool
		wantNil    bool
		wantActive bool
		wantStrict bool
	}{
		{
			name:    "no lock is not active",
			acquire: false,
			wantNil: true,
		},
		{
			name:      "non-runtime lock ignored",
			acquire:   true,
			key:       "checkout:owner/repo",
			pid:       4242,
			hostname:  host,
			expiresIn: time.Hour,
			pidAlive:  livePID,
			wantNil:   true,
		},
		{
			name:       "unexpired lease live same-host pid is strict-live",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  4 * time.Hour,
			pidAlive:   livePID,
			wantActive: true,
			wantStrict: true,
		},
		{
			name:       "unexpired lease dead pid is active but not strict-live",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  4 * time.Hour,
			pidAlive:   deadPID,
			wantActive: true,
			wantStrict: false,
		},
		{
			name:       "expired lease live pid is active but not strict-live",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  -time.Minute,
			pidAlive:   livePID,
			wantActive: true,
			wantStrict: false,
		},
		{
			name:       "expired lease dead pid is neither active nor strict-live",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  -time.Minute,
			pidAlive:   deadPID,
			wantActive: false,
			wantStrict: false,
		},
		{
			name:       "cross-host unexpired lease is strict-live (unverifiable)",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   "other-host",
			expiresIn:  4 * time.Hour,
			pidAlive:   deadPID,
			wantActive: true,
			wantStrict: true,
		},
		{
			name:       "empty hostname treated as this host",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   "",
			expiresIn:  4 * time.Hour,
			pidAlive:   livePID,
			wantActive: true,
			wantStrict: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir() + "/gitmoot.db")
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			if tc.acquire {
				acquired, err := store.AcquireResourceLock(ctx, ResourceLock{
					ResourceKey:   tc.key,
					OwnerJobID:    "job-1",
					OwnerToken:    "tok",
					OwnerPID:      tc.pid,
					OwnerHostname: tc.hostname,
					ExpiresAt:     now.Add(tc.expiresIn).Format(time.RFC3339Nano),
				}, now)
				if err != nil || !acquired {
					t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
				}
			}
			liveness, err := store.JobRuntimeLockLiveness(ctx, "job-1", now, host, tc.pidAlive)
			if err != nil {
				t.Fatalf("JobRuntimeLockLiveness returned error: %v", err)
			}
			if tc.wantNil {
				if liveness != nil {
					t.Fatalf("liveness = %+v, want nil", liveness)
				}
				return
			}
			if liveness == nil {
				t.Fatalf("liveness = nil, want non-nil")
			}
			if liveness.Active() != tc.wantActive {
				t.Fatalf("Active() = %v, want %v (%+v)", liveness.Active(), tc.wantActive, liveness)
			}
			if liveness.LiveAndUnexpired() != tc.wantStrict {
				t.Fatalf("LiveAndUnexpired() = %v, want %v (%+v)", liveness.LiveAndUnexpired(), tc.wantStrict, liveness)
			}
		})
	}
}
