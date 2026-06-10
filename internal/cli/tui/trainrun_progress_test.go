package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
)

func TestTrainRunLongPhaseShowsSpinnerElapsedAndHeader(t *testing.T) {
	snap := TrainRunSnapshot{
		SessionID:        "s",
		Phase:            "optimizer_running",
		PhaseStartedAt:   time.Now().Add(-5 * time.Minute),
		OptimizerBackend: "claude",
		OptimizerModel:   "claude-sonnet-4-6",
		OptimizerAttempt: "attempt-001",
	}
	m := trainRunModelWithDeps(t, TrainRunDeps{Load: func() (TrainRunSnapshot, error) { return snap, nil }}, snap)
	if !m.spinning {
		t.Fatal("a long-phase snapshot should arm the spinner")
	}
	view := m.View()
	for _, want := range []string{"optimizing", "elapsed 5m0s", "optimizer: claude · claude-sonnet-4-6 · attempt-001"} {
		if !strings.Contains(view, want) {
			t.Fatalf("long-phase view missing %q:\n%s", want, view)
		}
	}
}

func TestTrainRunSpinnerDisarmsOutsideLongPhases(t *testing.T) {
	long := TrainRunSnapshot{SessionID: "s", Phase: "optimizer_running"}
	m := trainRunModelWithDeps(t, TrainRunDeps{Load: func() (TrainRunSnapshot, error) { return long, nil }}, long)
	if !m.spinning {
		t.Fatal("long phase should arm the spinner")
	}
	next, _ := m.Update(trainSnapshotMsg{snap: TrainRunSnapshot{SessionID: "s", Phase: "candidate_created"}, at: time.Unix(2, 0)})
	m = next.(TrainRunModel)
	if m.spinning {
		t.Fatal("leaving the long phase should disarm the spinner")
	}
	// A stale spinner tick must not re-arm a dead chain.
	if _, cmd := m.Update(spinner.TickMsg{ID: m.spin.ID()}); cmd != nil {
		t.Fatal("stale spinner tick must be dropped")
	}
}

func TestTrainRunLiveElapsedFallsBackToSnapshotText(t *testing.T) {
	m := TrainRunModel{snap: TrainRunSnapshot{Elapsed: "2m"}}
	if got := m.liveElapsed(); got != "2m" {
		t.Fatalf("fallback elapsed = %q", got)
	}
}
