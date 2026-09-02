package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// noopDeliveryAdapter is a DeliveryAdapter that delivers nothing. Tests use it
// noopDeliveryAdapter is shared by tests that require an adapter but do not
// inspect its output.
type noopDeliveryAdapter struct{}

func (noopDeliveryAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, nil
}
