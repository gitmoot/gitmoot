package execbackend

import (
	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/execbackend/credentialplan"
)

// ErrCloudRuntimeUnsupported means a runtime has been deliberately classified
// as unable to consume brokered credentials from a remote execution backend.
var ErrCloudRuntimeUnsupported = credentialplan.ErrCloudRuntimeUnsupported

// RemoteCredentialPlan is the complete credential material a future remote
// backend may receive. Its defining package is separate from backend
// implementations, so they cannot populate or mutate its fields directly.
type RemoteCredentialPlan = credentialplan.Plan

// NewRemoteCredentialPlan constructs the only credential shape a future remote
// backend may consume. Accepting only a gateway-issued lease prevents callers
// from substituting a raw provider credential for broker material. No runtime
// is supported until a compatibility spike proves that runtime can target the
// mTLS broker directly.
func NewRemoteCredentialPlan(runtimeName string, lease *credgw.NetworkLease) (RemoteCredentialPlan, error) {
	return credentialplan.New(runtimeName, lease)
}

// RequireCloudRuntimeCredentialSupport is a closed classification switch over
// Gitmoot's compiled runtimes. There is intentionally no default arm: a runtime
// absent from the switch is unclassified, which is distinct from an explicit
// unsupported decision and makes the registry-enumeration test fail.
func RequireCloudRuntimeCredentialSupport(runtimeName string) error {
	return credentialplan.RequireCloudRuntimeSupport(runtimeName)
}
