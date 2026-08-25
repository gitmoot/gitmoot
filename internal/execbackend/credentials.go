package execbackend

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// ErrCloudRuntimeUnsupported means a runtime has been deliberately classified
// as unable to consume brokered credentials from a remote execution backend.
var ErrCloudRuntimeUnsupported = errors.New("cloud runtime does not support brokered credentials")

// BrokerCapability is a short-lived credential-gateway capability.
type BrokerCapability string

// RemoteCredentialPlan is the complete credential material a future remote
// backend may receive. Its unexported shape and constructor deliberately leave
// no field or parameter for a raw provider credential.
type RemoteCredentialPlan struct {
	endpoint          url.URL
	capability        BrokerCapability
	clientCertificate tls.Certificate
}

// NewRemoteCredentialPlan constructs the only credential shape a future remote
// backend may consume. Accepting only a gateway-issued lease prevents callers
// from substituting a raw provider credential for broker material. No runtime
// is supported until a compatibility spike proves that runtime can target the
// mTLS broker directly.
func NewRemoteCredentialPlan(runtimeName string, lease *credgw.NetworkLease) (RemoteCredentialPlan, error) {
	if err := RequireCloudRuntimeCredentialSupport(runtimeName); err != nil {
		return RemoteCredentialPlan{}, err
	}
	if lease == nil {
		return RemoteCredentialPlan{}, errors.New("remote credential plan requires a broker lease")
	}
	endpoint, err := url.Parse(lease.URL())
	if err != nil {
		return RemoteCredentialPlan{}, fmt.Errorf("parse broker lease endpoint: %w", err)
	}
	return RemoteCredentialPlan{
		endpoint:          *endpoint,
		capability:        BrokerCapability(lease.Capability()),
		clientCertificate: lease.ClientCertificate(),
	}, nil
}

// RequireCloudRuntimeCredentialSupport is a closed classification switch over
// Gitmoot's compiled runtimes. There is intentionally no default arm: a runtime
// absent from the switch is unclassified, which is distinct from an explicit
// unsupported decision and makes the registry-enumeration test fail.
func RequireCloudRuntimeCredentialSupport(runtimeName string) error {
	runtimeName = strings.TrimSpace(runtimeName)
	switch runtimeName {
	case runtime.CodexRuntime:
		return unsupportedCloudRuntime(runtimeName)
	case runtime.ClaudeRuntime:
		return unsupportedCloudRuntime(runtimeName)
	case runtime.KimiRuntime:
		return unsupportedCloudRuntime(runtimeName)
	case runtime.KimiCLIRuntime:
		return unsupportedCloudRuntime(runtimeName)
	case runtime.OmpRuntime:
		return unsupportedCloudRuntime(runtimeName)
	case runtime.ShellRuntime:
		return unsupportedCloudRuntime(runtimeName)
	}
	return fmt.Errorf("cloud runtime %q has no broker credential classification", runtimeName)
}

func unsupportedCloudRuntime(runtimeName string) error {
	return fmt.Errorf("%w: runtime %q has not passed broker compatibility verification", ErrCloudRuntimeUnsupported, runtimeName)
}
