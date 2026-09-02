package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// noopDeliveryAdapter is a DeliveryAdapter that delivers nothing. Tests use it
// when the adapter must exist but its output is irrelevant. It was named
// cockpitStubAdapter and lived in the cockpit test file deleted by #1753; the
// surviving users have nothing to do with the cockpit.
type noopDeliveryAdapter struct{}

func (noopDeliveryAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, nil
}
