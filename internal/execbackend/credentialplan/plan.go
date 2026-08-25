// Package credentialplan owns the opaque credential shape available to a
// future remote execution backend.
package credentialplan

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

type brokerCapability string

// Plan is the complete credential material a future remote backend may
// receive. Its fields are owned by this package so backend implementations in
// package execbackend cannot construct or mutate them directly.
type Plan struct {
	endpoint          url.URL
	capability        brokerCapability
	clientCertificate tls.Certificate
}

// New constructs a Plan exclusively from a gateway-issued lease. No runtime
// is supported until a compatibility spike proves it can target the mTLS
// broker directly.
func New(runtimeName string, lease *credgw.NetworkLease) (Plan, error) {
	if err := RequireCloudRuntimeSupport(runtimeName); err != nil {
		return Plan{}, err
	}
	if lease == nil {
		return Plan{}, errors.New("remote credential plan requires a broker lease")
	}
	endpoint, err := url.Parse(lease.URL())
	if err != nil {
		return Plan{}, fmt.Errorf("parse broker lease endpoint: %w", err)
	}
	return Plan{
		endpoint:          *endpoint,
		capability:        brokerCapability(lease.Capability()),
		clientCertificate: cloneCertificate(lease.ClientCertificate()),
	}, nil
}

// Endpoint returns a copy of the broker route URL.
func (p Plan) Endpoint() url.URL {
	return p.endpoint
}

// Capability returns the short-lived credential-gateway capability.
func (p Plan) Capability() string {
	return string(p.capability)
}

// ClientCertificate returns a copy of the per-job mTLS client certificate.
func (p Plan) ClientCertificate() tls.Certificate {
	return cloneCertificate(p.clientCertificate)
}

func cloneCertificate(certificate tls.Certificate) tls.Certificate {
	clone := certificate
	clone.Certificate = make([][]byte, len(certificate.Certificate))
	for i, der := range certificate.Certificate {
		clone.Certificate[i] = append([]byte(nil), der...)
	}
	clone.OCSPStaple = append([]byte(nil), certificate.OCSPStaple...)
	clone.SignedCertificateTimestamps = make([][]byte, len(certificate.SignedCertificateTimestamps))
	for i, timestamp := range certificate.SignedCertificateTimestamps {
		clone.SignedCertificateTimestamps[i] = append([]byte(nil), timestamp...)
	}
	clone.SupportedSignatureAlgorithms = append([]tls.SignatureScheme(nil), certificate.SupportedSignatureAlgorithms...)
	return clone
}

// RequireCloudRuntimeSupport is a closed classification switch over Gitmoot's
// compiled runtimes. There is intentionally no default arm: an absent runtime
// is unclassified rather than inheriting support or an existing decision.
func RequireCloudRuntimeSupport(runtimeName string) error {
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
