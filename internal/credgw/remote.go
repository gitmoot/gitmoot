package credgw

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RemoteListenerOptions configures the independently authenticated listener
// reachable from execution sandboxes. ListenAddress is the local bind address.
// AdvertiseURLs are the HTTPS origins sandboxes use to reach it; they may differ
// from the bind address and from each other when a load balancer or a reverse
// tunnel forwards a public endpoint, so one listener can serve sandbox
// providers that reach it by different routes. The first entry is the primary
// origin, used by leases that name none. An empty list advertises
// https://<listener address>. Every entry must be an HTTPS origin; blank
// entries and duplicate origins are refused rather than silently dropped.
type RemoteListenerOptions struct {
	ListenAddress string
	AdvertiseURLs []string
}

// RemoteMaterial contains only job-scoped broker credentials. It never
// contains an upstream provider key. String and GoString deliberately redact
// the client private key and capability if the value reaches diagnostics.
type RemoteMaterial struct {
	URL               string
	Capability        string
	Placeholder       string
	CACertificate     []byte
	ClientCertificate []byte
	ClientPrivateKey  []byte
}

func (RemoteMaterial) String() string   { return "[REDACTED]" }
func (RemoteMaterial) GoString() string { return "[REDACTED]" }

// CurlConfig returns a credential file suitable for curl --config. The caller
// supplies sandbox paths, so neither the capability nor the private key needs
// to appear in argv, process listings, or environment variables.
func (m RemoteMaterial) CurlConfig(caPath, certificatePath, privateKeyPath string) []byte {
	lines := []string{
		"cacert = " + strconv.Quote(caPath),
		"cert = " + strconv.Quote(certificatePath),
		"key = " + strconv.Quote(privateKeyPath),
		"header = " + strconv.Quote(CapabilityHeader+": "+m.Capability),
		"header = " + strconv.Quote("Authorization: Bearer "+m.Placeholder),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

type certificateAuthority struct {
	certificate    *x509.Certificate
	privateKey     crypto.Signer
	certificatePEM []byte
}

// EnableRemote adds the mTLS listener without changing the loopback listener.
// Client certificates are issued from an in-memory process CA and are useful
// only while this Gateway process and the matching lease are alive.
func (g *Gateway) EnableRemote(options RemoteListenerOptions) error {
	if g == nil {
		return errors.New("credential gateway is not running")
	}
	configuredOptions := normalizedRemoteListenerOptions(options)
	listenAddress := configuredOptions.ListenAddress
	if listenAddress == "" {
		return errors.New("remote credential gateway listen address is required")
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen for remote credential gateway: %w", err)
	}
	origins, err := remoteAdvertiseOrigins(configuredOptions.AdvertiseURLs, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return err
	}
	ca, err := newCertificateAuthority()
	if err != nil {
		_ = listener.Close()
		return err
	}
	hosts := make([]string, len(origins))
	for index, origin := range origins {
		hosts[index] = origin.Hostname()
	}
	serverCertificate, err := ca.issueServer(hosts)
	if err != nil {
		_ = listener.Close()
		return err
	}
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.certificate)
	tlsListener := tls.NewListener(listener, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientRoots,
	})
	server := &http.Server{
		Handler:           remoteGatewayHandler{gateway: g},
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = listener.Close()
		return errors.New("credential gateway is not running")
	}
	if g.remoteServer != nil {
		g.mu.Unlock()
		_ = listener.Close()
		return errors.New("remote credential gateway is already configured")
	}
	g.remoteListener = tlsListener
	g.remoteServer = server
	g.remoteOrigins = origins
	g.remoteCA = ca
	g.remoteOptions = configuredOptions
	g.mu.Unlock()
	go func() { _ = server.Serve(tlsListener) }()
	return nil
}

func normalizedRemoteListenerOptions(options RemoteListenerOptions) RemoteListenerOptions {
	normalized := RemoteListenerOptions{ListenAddress: strings.TrimSpace(options.ListenAddress)}
	for _, raw := range options.AdvertiseURLs {
		normalized.AdvertiseURLs = append(normalized.AdvertiseURLs, strings.TrimSpace(raw))
	}
	return normalized
}

// remoteConfiguredFor allows immutable listener reuse only when a later
// dispatch requested the same coordinates. Rebinding would invalidate active
// per-job certificates, so changed coordinates fail loudly until restart.
func (g *Gateway) remoteConfiguredFor(options RemoteListenerOptions) (bool, error) {
	if g == nil {
		return false, errors.New("credential gateway is not running")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.remoteServer == nil {
		return false, nil
	}
	requested := normalizedRemoteListenerOptions(options)
	if g.remoteOptions.ListenAddress != requested.ListenAddress || !slices.Equal(g.remoteOptions.AdvertiseURLs, requested.AdvertiseURLs) {
		return true, errors.New("remote credential gateway configuration changed; restart the daemon to apply listener coordinates")
	}
	return true, nil
}

// RemoteURL returns the primary advertised origin, or "" when the remote
// listener is not configured.
func (g *Gateway) RemoteURL() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.remoteOrigins) == 0 {
		return ""
	}
	return g.remoteOrigins[0].String()
}

// remoteOriginFor resolves a remote lease's requested origin against the
// advertised set: empty selects the primary origin, anything not advertised is
// refused so a lease never hands a sandbox a URL the server certificate does
// not cover. The caller holds g.mu.
func (g *Gateway) remoteOriginFor(advertiseURL string) (*url.URL, error) {
	if advertiseURL == "" {
		return g.remoteOrigins[0], nil
	}
	for _, origin := range g.remoteOrigins {
		if strings.EqualFold(origin.String(), advertiseURL) {
			return origin, nil
		}
	}
	return nil, fmt.Errorf("remote credential gateway does not advertise %q", advertiseURL)
}

// remoteRequestRefusalReason names the precondition a remote gateway request
// failed, or "" when it passed. Kept separate so the reasons are enumerable and
// testable rather than implied by a compound boolean.
func remoteRequestRefusalReason(routed bool, r *http.Request) string {
	switch {
	case !routed:
		return "request path is not a proxy route"
	case r.TLS == nil:
		return "connection is not TLS"
	case len(r.TLS.PeerCertificates) == 0:
		return "client presented no certificate"
	// A CHAIN IS ACCEPTED, and it has to be. The check here previously demanded
	// EXACTLY ONE peer certificate, which no real client could satisfy through
	// the material this gateway itself installs: curl in an E2B sandbox, given
	// both `cert` and `cacert`, presents the leaf AND the CA, so every remote
	// request was refused with a bare 401 and no record of why.
	//
	// Requiring a bare leaf was never the security property. RequireAndVerify
	// ClientCert already verifies the chain against the process CA, and the
	// identity check below pins the sha256 of PeerCertificates[0] - the LEAF,
	// which TLS guarantees is first - against the exact certificate issued for
	// this sandbox. Extra chain members change neither of those.
	default:
		return ""
	}
}

type remoteGatewayHandler struct {
	gateway *Gateway
}

func (h remoteGatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EVERY REFUSAL MUST SAY WHICH CHECK REFUSED. This branch previously
	// returned 401 and logged NOTHING, so a sandbox that reached the listener
	// and was rejected left no record anywhere: the operator saw only the body
	// "unauthorized" inside the sandbox, with no way to tell a routing mistake
	// from a missing client certificate from a presented chain.
	//
	// Found while bringing up the first real remote review. The reason is coarse
	// on purpose - it names the failed precondition and never echoes certificate
	// contents, subject names, or the capability.
	route, routed := proxyRoute(r.URL.EscapedPath())
	if reason := remoteRequestRefusalReason(routed, r); reason != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		h.gateway.logRemoteRefusal(r.Method, reason)
		return
	}
	capability := strings.TrimSpace(r.Header.Get(CapabilityHeader))
	if !validCapabilitySyntax(capability) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		// Distinguish absent from malformed. Both were logged identically as a
		// bare status=401, which cannot tell a client that never set the header
		// from one whose value is the wrong shape.
		if capability == "" {
			h.gateway.logRemoteRefusal(r.Method, "capability header absent")
		} else {
			h.gateway.logRemoteRefusal(r.Method, "capability header malformed")
		}
		return
	}
	// The Host check against the lease's own advertised origin happens in
	// serveProxyRequest, which has the registered lease in hand.
	h.gateway.serveProxyRequest(w, r, proxyRequestAccess{
		route: route, capability: capability, suffixRoute: route, remote: true,
		clientCertificate: sha256.Sum256(r.TLS.PeerCertificates[0].Raw),
	})
}

func validCapabilitySyntax(capability string) bool {
	if len(capability) != proxyCapabilityBytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(capability)
	return err == nil && len(decoded) == proxyCapabilityBytes
}

// remoteAdvertiseOrigins validates the advertised origins in order. An empty
// list advertises the listener's own address.
func remoteAdvertiseOrigins(raws []string, address net.Addr) ([]*url.URL, error) {
	if len(raws) == 0 {
		raws = []string{"https://" + address.String()}
	}
	origins := make([]*url.URL, 0, len(raws))
	for _, raw := range raws {
		origin, err := remoteAdvertiseURL(raw)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(origins, func(existing *url.URL) bool { return strings.EqualFold(existing.Host, origin.Host) }) {
			return nil, fmt.Errorf("invalid remote credential gateway URL %q: duplicate advertised origin", raw)
		}
		origins = append(origins, origin)
	}
	return origins, nil
}

func remoteAdvertiseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return nil, fmt.Errorf("invalid remote credential gateway URL %q: require an HTTPS origin", raw)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid remote credential gateway URL %q: path, query, and fragment are not allowed", raw)
	}
	return parsed, nil
}

func newCertificateAuthority() (*certificateAuthority, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate remote credential gateway CA: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: newCertificateSerial(), Subject: pkix.Name{CommonName: "gitmoot credential gateway"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create remote credential gateway CA: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse remote credential gateway CA: %w", err)
	}
	return &certificateAuthority{certificate: certificate, privateKey: privateKey,
		certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// issueServer issues one leaf covering every advertised host: an IP SAN for
// addresses and a DNS SAN otherwise. The first host is the subject name.
func (ca *certificateAuthority) issueServer(hosts []string) (tls.Certificate, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate remote credential gateway server key: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: newCertificateSerial(), Subject: pkix.Name{CommonName: hosts[0]},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 1, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			if !slices.ContainsFunc(template.IPAddresses, ip.Equal) {
				template.IPAddresses = append(template.IPAddresses, ip)
			}
		} else if !slices.ContainsFunc(template.DNSNames, func(name string) bool { return strings.EqualFold(name, host) }) {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, publicKey, ca.privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create remote credential gateway server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("encode remote credential gateway server key: %w", err)
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
}

func (ca *certificateAuthority) issueClient(sandboxID, runtimeName string, expiresAt time.Time) (RemoteMaterial, [sha256.Size]byte, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return RemoteMaterial{}, [sha256.Size]byte{}, fmt.Errorf("generate remote credential gateway client key: %w", err)
	}
	now := time.Now().UTC()
	identity, _ := url.Parse("spiffe://gitmoot/sandbox/" + url.PathEscape(sandboxID) + "/runtime/" + url.PathEscape(runtimeName))
	template := &x509.Certificate{
		SerialNumber: newCertificateSerial(), Subject: pkix.Name{CommonName: sandboxID + ":" + runtimeName},
		NotBefore: now.Add(-time.Minute), NotAfter: expiresAt.UTC(),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{identity},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, publicKey, ca.privateKey)
	if err != nil {
		return RemoteMaterial{}, [sha256.Size]byte{}, fmt.Errorf("create remote credential gateway client certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return RemoteMaterial{}, [sha256.Size]byte{}, fmt.Errorf("encode remote credential gateway client key: %w", err)
	}
	return RemoteMaterial{
		CACertificate:     append([]byte(nil), ca.certificatePEM...),
		ClientCertificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		ClientPrivateKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, sha256.Sum256(der), nil
}

func newCertificateSerial() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return serial
}
