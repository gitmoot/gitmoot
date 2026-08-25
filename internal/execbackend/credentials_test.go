package execbackend

import (
	"crypto/tls"
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// This unkeyed compile-time assertion pins the plan to exactly these three
// broker-material fields. Adding a raw-key field makes this package fail to
// compile instead of silently widening the remote credential surface.
var _ = RemoteCredentialPlan{url.URL{}, BrokerCapability("capability"), tls.Certificate{}}
var _ func(string, *credgw.NetworkLease) (RemoteCredentialPlan, error) = NewRemoteCredentialPlan

func TestRemoteCredentialPlanCarriesOnlyBrokerMaterial(t *testing.T) {
	typeOfPlan := reflect.TypeFor[RemoteCredentialPlan]()
	want := []string{"endpoint", "capability", "clientCertificate"}
	if typeOfPlan.NumField() != len(want) {
		t.Fatalf("RemoteCredentialPlan fields = %d, want exactly %d broker fields", typeOfPlan.NumField(), len(want))
	}
	for i, name := range want {
		if got := typeOfPlan.Field(i).Name; got != name {
			t.Fatalf("RemoteCredentialPlan field %d = %q, want %q", i, got, name)
		}
	}
}

func TestCloudCredentialSupportClassifiesEveryRegisteredRuntime(t *testing.T) {
	runtimes := runtime.SupportedRuntimes()
	if len(runtimes) == 0 {
		t.Fatal("runtime registry is empty")
	}
	for _, runtimeName := range runtimes {
		err := RequireCloudRuntimeCredentialSupport(runtimeName)
		if !errors.Is(err, ErrCloudRuntimeUnsupported) {
			t.Errorf("runtime %q classification error = %v; want ErrCloudRuntimeUnsupported", runtimeName, err)
		}
	}
}

func TestRemoteCredentialPlanRejectsUnsupportedAndUnclassifiedRuntimes(t *testing.T) {
	if _, err := NewRemoteCredentialPlan(runtime.ClaudeRuntime, nil); !errors.Is(err, ErrCloudRuntimeUnsupported) {
		t.Fatalf("Claude remote credential plan error = %v, want ErrCloudRuntimeUnsupported", err)
	}
	if _, err := NewRemoteCredentialPlan("future-runtime", nil); err == nil || errors.Is(err, ErrCloudRuntimeUnsupported) {
		t.Fatalf("unclassified remote credential plan error = %v, want distinct fail-closed classification error", err)
	}
}
