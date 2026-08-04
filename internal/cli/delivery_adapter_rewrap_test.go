package cli

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// deliveryAdapterForm is one concrete shape a composed adapter can reach the
// rewrap switches in: the VALUE the Factory hands back, and the POINTER form the
// switches carry a separate case for.
type deliveryAdapterForm struct {
	label   string
	adapter workflow.DeliveryAdapter
}

// deliveryAdapterForms returns the value and pointer forms of a Factory-built
// adapter. The pointer is synthesized reflectively rather than named per runtime,
// so a newly registered runtime is covered here the moment it joins
// SupportedRuntimes — the point of the test is that nobody has to remember.
func deliveryAdapterForms(t *testing.T, adapter runtime.Adapter) []deliveryAdapterForm {
	t.Helper()
	value, ok := adapter.(workflow.DeliveryAdapter)
	if !ok {
		t.Fatalf("adapter %T does not satisfy workflow.DeliveryAdapter", adapter)
	}
	box := reflect.New(reflect.TypeOf(adapter))
	box.Elem().Set(reflect.ValueOf(adapter))
	pointer, ok := box.Interface().(workflow.DeliveryAdapter)
	if !ok {
		t.Fatalf("*%T does not satisfy workflow.DeliveryAdapter", adapter)
	}
	return []deliveryAdapterForm{{"value", value}, {"pointer", pointer}}
}

// TestEveryRuntimeAdapterSurvivesDeliveryRewrap is the FAIL-OPEN tripwire for the
// two per-adapter-type switches that rewrap an already-composed delivery adapter:
// appendDeliveryAdapterOutput (transcript_retention.go, the transcript tee) and
// injectDeliveryAdapterEnv (isolated_tool_cache.go, the tool-cache env). Both end
// in a `default:` that returns an error, so a runtime missing a case does not
// degrade quietly — it breaks transcript retention and tool-cache isolation for
// every job on that runtime, and only at delivery time.
//
// Adding a runtime to the Factory is therefore not enough; it must be listed in
// BOTH switches, in BOTH the value and pointer forms (the daemon composes either).
// This walks SupportedRuntimes instead of naming runtimes, so the next runtime is
// covered without editing this test.
func TestEveryRuntimeAdapterSurvivesDeliveryRewrap(t *testing.T) {
	names := runtime.SupportedRuntimes()
	if len(names) == 0 {
		t.Fatal("SupportedRuntimes is empty; this test would pass vacuously")
	}
	// Non-vacuity: omp (#1428) is the runtime this tripwire was written for, so a
	// walk that silently stopped covering it is a failure, not a pass.
	sawOmp := false
	for _, name := range names {
		if name == runtime.OmpRuntime {
			sawOmp = true
		}
		t.Run(name, func(t *testing.T) {
			adapter, err := (runtime.Factory{}).Adapter(name)
			if err != nil {
				t.Fatalf("Factory.Adapter(%q) returned error: %v", name, err)
			}
			// Fresh forms per switch so neither wrap observes the other's mutation.
			for _, form := range deliveryAdapterForms(t, adapter) {
				wrapped, err := appendDeliveryAdapterOutput(form.adapter, &bytes.Buffer{})
				if err != nil {
					t.Fatalf("appendDeliveryAdapterOutput(%s %T) returned error: %v; every %q job would lose transcript retention", form.label, form.adapter, err, name)
				}
				if wrapped == nil {
					t.Fatalf("appendDeliveryAdapterOutput(%s %T) returned a nil adapter", form.label, form.adapter)
				}
			}
			for _, form := range deliveryAdapterForms(t, adapter) {
				wrapped, err := injectDeliveryAdapterEnv(form.adapter, []string{"GITMOOT_TEST_REWRAP_PROBE=1"})
				if err != nil {
					t.Fatalf("injectDeliveryAdapterEnv(%s %T) returned error: %v; every %q job would lose tool-cache isolation", form.label, form.adapter, err, name)
				}
				if wrapped == nil {
					t.Fatalf("injectDeliveryAdapterEnv(%s %T) returned a nil adapter", form.label, form.adapter)
				}
			}
		})
	}
	if !sawOmp {
		t.Fatalf("omp must be among the walked runtimes: %v", names)
	}
}
