package execbackend

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

var _ func(string, *credgw.NetworkLease) (RemoteCredentialPlan, error) = NewRemoteCredentialPlan

func TestRemoteCredentialPlanCarriesOnlyBrokerMaterial(t *testing.T) {
	typeOfPlan := reflect.TypeFor[RemoteCredentialPlan]()
	want := []string{"endpoint", "capability", "clientCertificate"}
	const owner = "github.com/gitmoot/gitmoot/internal/execbackend/credentialplan"
	if got := typeOfPlan.PkgPath(); got != owner {
		t.Fatalf("RemoteCredentialPlan owner = %q, want isolated package %q", got, owner)
	}
	if typeOfPlan.NumField() != len(want) {
		t.Fatalf("RemoteCredentialPlan fields = %d, want exactly %d broker fields", typeOfPlan.NumField(), len(want))
	}
	for i, name := range want {
		field := typeOfPlan.Field(i)
		if got := field.Name; got != name {
			t.Fatalf("RemoteCredentialPlan field %d = %q, want %q", i, got, name)
		}
		if field.PkgPath != owner {
			t.Fatalf("RemoteCredentialPlan field %q is exported or owned by %q, want private owner %q", name, field.PkgPath, owner)
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
