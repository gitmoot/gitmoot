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
		exclude    string
		wantNil    bool
		wantActive bool
		wantStrict bool
		wantLease  bool
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
			wantLease:  true,
		},
		{
			// The daemon-restart shape: an unexpired lease but a DEAD owner PID (the
			// prior daemon). NOT strict-live (PID dead), but the lease is still held —
			// recovery must honor the lease and leave the job running (#536).
			name:       "unexpired lease dead pid is active and lease-held but not strict-live",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  4 * time.Hour,
			pidAlive:   deadPID,
			wantActive: true,
			wantStrict: false,
			wantLease:  true,
		},
		{
			// PID reuse guard (#536 finding 5): a live same-host PID with an EXPIRED
			// lease is far more likely a recycled PID than the original worker (the
			// lock records the daemon PID), so it must NOT keep the lock active and
			// block reclamation. Not active, not strict-live, not lease-held.
			name:       "expired lease live pid is neither active nor strict-live nor lease-held",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  -time.Minute,
			pidAlive:   livePID,
			wantActive: false,
			wantStrict: false,
			wantLease:  false,
		},
		{
			name:       "expired lease dead pid is neither active nor strict-live nor lease-held",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   host,
			expiresIn:  -time.Minute,
			pidAlive:   deadPID,
			wantActive: false,
			wantStrict: false,
			wantLease:  false,
		},
		{
			name:       "cross-host unexpired lease is strict-live and lease-held (unverifiable)",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   "other-host",
			expiresIn:  4 * time.Hour,
			pidAlive:   deadPID,
			wantActive: true,
			wantStrict: true,
			wantLease:  true,
		},
		{
			// Cross-host strand guard (#536 finding 2): after a daemon restart under a
			// DIFFERENT hostname (k8s/docker pod recreate), the pre-restart lock is
			// cross-host. Once its LEASE expires it is almost always our own dead self,
			// so it must become reclaimable — NOT active and NOT lease-held — or the job
			// is never requeued and its worktree never cleaned (permanent strand). An
			// UNEXPIRED cross-host lock (the case above) is still protected.
			name:       "cross-host expired lease is reclaimable (not active, not lease-held)",
			acquire:    true,
			key:        "runtime:codex:s1",
			pid:        4242,
			hostname:   "other-host",
			expiresIn:  -time.Minute,
			pidAlive:   deadPID,
			wantActive: false,
			wantStrict: false,
			wantLease:  false,
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
			wantLease:  true,
		},
		{
			// excludeOwnerToken drops the caller's OWN held lock: a run finishing
			// normally still holds its runtime-session lock (released after AdvanceJob),
			// so excluding it by token yields nil — cleanup proceeds (#536).
			name:      "self-owned lock excluded by token yields nil",
			acquire:   true,
			key:       "runtime:codex:s1",
			pid:       4242,
			hostname:  host,
			expiresIn: 4 * time.Hour,
			pidAlive:  livePID,
			exclude:   "tok",
			wantNil:   true,
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
			liveness, err := store.JobRuntimeLockLiveness(ctx, "job-1", now, host, tc.pidAlive, tc.exclude)
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
			if liveness.LeaseHeld() != tc.wantLease {
				t.Fatalf("LeaseHeld() = %v, want %v (%+v)", liveness.LeaseHeld(), tc.wantLease, liveness)
			}
		})
	}
}
