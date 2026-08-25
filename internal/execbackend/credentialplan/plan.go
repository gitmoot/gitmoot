// Package credentialplan owns the opaque credential shape available to a
// future remote execution backend.
package credentialplan

import (
	"crypto/tls"
	"crypto/x509"
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
	clientCertificate clientCertificateMaterial
}

type clientCertificateMaterial struct {
	certificateDER               [][]byte
	privateKeyPKCS8              []byte
	ocspStaple                   []byte
	signedCertificateTimestamps  [][]byte
	supportedSignatureAlgorithms []tls.SignatureScheme
}

// New constructs a Plan exclusively from a gateway-issued lease. No runtime
// is supported until a compatibility spike proves it can target the mTLS
// broker directly.
func New(runtimeName string, lease *credgw.NetworkLease) (Plan, error) {
	if err := RequireCloudRuntimeSupport(runtimeName); err != nil {
		return Plan{}, err
	}
	return newFromNetworkLease(lease)
}

func newFromNetworkLease(lease *credgw.NetworkLease) (Plan, error) {
	if lease == nil {
		return Plan{}, errors.New("remote credential plan requires a broker lease")
	}
	endpoint, err := url.Parse(lease.URL())
	if err != nil {
		return Plan{}, fmt.Errorf("parse broker lease endpoint: %w", err)
	}
	certificate, err := freezeClientCertificate(lease.ClientCertificate())
	if err != nil {
		return Plan{}, fmt.Errorf("freeze broker lease client certificate: %w", err)
	}
	return Plan{
		endpoint:          *endpoint,
		capability:        brokerCapability(lease.Capability()),
		clientCertificate: certificate,
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

// ClientTLSConfig returns a TLS configuration that can use the per-job client
// identity without exposing the plan's stored key material. Each handshake
// receives independently parsed certificate and private-key objects.
func (p Plan) ClientTLSConfig() (*tls.Config, error) {
	serverName := p.endpoint.Hostname()
	if serverName == "" {
		return nil, errors.New("broker endpoint has no TLS server name")
	}
	material := p.clientCertificate
	if _, err := material.certificate(); err != nil {
		return nil, fmt.Errorf("prepare broker client certificate: %w", err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return material.certificate()
		},
	}, nil
}

func freezeClientCertificate(certificate tls.Certificate) (clientCertificateMaterial, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return clientCertificateMaterial{}, errors.New("client certificate is incomplete")
	}
	privateKeyPKCS8, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		return clientCertificateMaterial{}, fmt.Errorf("marshal client private key: %w", err)
	}
	material := clientCertificateMaterial{
		certificateDER:               cloneByteSlices(certificate.Certificate),
		privateKeyPKCS8:              append([]byte(nil), privateKeyPKCS8...),
		ocspStaple:                   append([]byte(nil), certificate.OCSPStaple...),
		signedCertificateTimestamps:  cloneByteSlices(certificate.SignedCertificateTimestamps),
		supportedSignatureAlgorithms: append([]tls.SignatureScheme(nil), certificate.SupportedSignatureAlgorithms...),
	}
	return material, nil
}

func (m clientCertificateMaterial) certificate() (*tls.Certificate, error) {
	if len(m.certificateDER) == 0 || len(m.privateKeyPKCS8) == 0 {
		return nil, errors.New("client certificate material is incomplete")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(m.privateKeyPKCS8)
	if err != nil {
		return nil, fmt.Errorf("parse client private key: %w", err)
	}
	certificateDER := cloneByteSlices(m.certificateDER)
	leaf, err := x509.ParseCertificate(certificateDER[0])
	if err != nil {
		return nil, fmt.Errorf("parse client leaf certificate: %w", err)
	}
	return &tls.Certificate{
		Certificate:                  certificateDER,
		PrivateKey:                   privateKey,
		OCSPStaple:                   append([]byte(nil), m.ocspStaple...),
		SignedCertificateTimestamps:  cloneByteSlices(m.signedCertificateTimestamps),
		Leaf:                         leaf,
		SupportedSignatureAlgorithms: append([]tls.SignatureScheme(nil), m.supportedSignatureAlgorithms...),
	}, nil
}

func cloneByteSlices(values [][]byte) [][]byte {
	clone := make([][]byte, len(values))
	for i, value := range values {
		clone[i] = append([]byte(nil), value...)
	}
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
