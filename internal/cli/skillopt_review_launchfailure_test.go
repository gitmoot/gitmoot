package cli

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestIsExecLaunchFailure(t *testing.T) {
	// A real missing-binary exec surfaces exec.ErrNotFound in its chain.
	_, realNotFound := exec.Command("gitmoot-nonexistent-binary-xyz-734").Output()
	if realNotFound == nil {
		t.Fatal("expected a launch failure exec'ing a missing binary")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"real exec not found", realNotFound, true},
		{"fork/exec ARG_MAX", errors.New("fork/exec /usr/bin/gh: argument list too long"), true},
		{"argument list too long only", errors.New("argument list too long"), true},
		{"wrapped ErrNotFound", errors.Join(errors.New("gh call failed"), exec.ErrNotFound), true},
		{"http 422 validation", errors.New("HTTP 422: Validation Failed"), false},
		{"mid-flight timeout", errors.New("context deadline exceeded"), false},
		{"killed mid-flight", errors.New("signal: killed"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecLaunchFailure(tc.err); got != tc.want {
				t.Fatalf("isExecLaunchFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDowngradeReviewMarkerOnLaunchFailure asserts the posting_external latch is
// cleared only when the gh publish never launched, and preserved for every
// genuinely ambiguous failure.
func TestDowngradeReviewMarkerOnLaunchFailure(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	session := db.SkillOptTrainSession{ID: "sess-734"}
	iteration := db.SkillOptTrainIteration{ID: "iter-734"}
	posting := map[string]any{
		"status":                   "posting_external",
		"repo":                     "owner/reviews",
		"external_post_started_at": "2026-07-08T00:00:00Z",
	}

	writePosting := func(t *testing.T) {
		t.Helper()
		if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, posting); err != nil {
			t.Fatalf("write posting_external marker: %v", err)
		}
	}
	markerStatus := func(t *testing.T) string {
		t.Helper()
		review, ok, err := readSkillOptTrainReviewRecovery(paths, session, iteration)
		if err != nil {
			t.Fatalf("read recovery marker: %v", err)
		}
		if !ok {
			return "<absent>"
		}
		return metadataString(review, "status")
	}

	t.Run("exec-launch failure clears the latch and permits a retry", func(t *testing.T) {
		writePosting(t)
		launchErr := errors.New("fork/exec /usr/bin/gh: argument list too long")
		downgradeReviewMarkerOnLaunchFailure(paths, session, iteration, posting, launchErr)

		if got := markerStatus(t); got != "failed_pre_exec" {
			t.Fatalf("marker status after launch failure = %q, want failed_pre_exec", got)
		}
		// recover must now let the next attempt proceed fresh (no false latch).
		_, ok, err := recoverSkillOptTrainReviewPublication(context.Background(), paths, (*db.Store)(nil), session, iteration)
		if err != nil {
			t.Fatalf("recover after downgrade returned error: %v", err)
		}
		if ok {
			t.Fatal("recover latched a failed_pre_exec marker; a retry must be permitted")
		}
	})

	t.Run("mid-flight failure preserves the conservative latch", func(t *testing.T) {
		writePosting(t)
		midFlightErr := errors.New("context deadline exceeded")
		downgradeReviewMarkerOnLaunchFailure(paths, session, iteration, posting, midFlightErr)

		if got := markerStatus(t); got != "posting_external" {
			t.Fatalf("marker status after mid-flight failure = %q, want posting_external", got)
		}
		// recover must still latch: the post may have reached GitHub.
		_, ok, err := recoverSkillOptTrainReviewPublication(context.Background(), paths, (*db.Store)(nil), session, iteration)
		if err == nil {
			t.Fatal("recover did not latch posting_external; duplicate-issue guard lost")
		}
		if !ok {
			t.Fatal("recover should report ok=true when latched")
		}
	})
}
