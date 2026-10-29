package cli

// tmp red-on-old stubs: the pre-#2359 code has no janitor consumer; these
// compile the new tests against it so they can fail on behaviour.
import (
	"context"
	"errors"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

const janitorPressureHeldEventKind = "janitor_pressure_held"

var runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) { return nil, nil }

type janitorCapacitySnapshot struct{}

func (janitorCapacitySnapshot) decide(map[uint64]string) string { return "" }

func parseJanitorCapacity([]byte, string, time.Time) (janitorCapacitySnapshot, error) {
	return janitorCapacitySnapshot{}, errors.New("no consumer")
}

func janitorHostDevices(p []string) (map[uint64]string, error) { return map[uint64]string{1: "x"}, nil }

func janitorPressureQueuedDispatch(_ context.Context, _ jobWorker, jobs []db.Job, _, _ string) []db.Job {
	return jobs
}
