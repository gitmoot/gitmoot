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
	"strconv"
	"strings"
	"time"
)

// RemoteListenerOptions configures the independently authenticated listener
// reachable from execution sandboxes. AdvertiseURL is the HTTPS origin the
// sandbox uses; ListenAddress is the local bind address and may differ when a
// load balancer forwards the public endpoint.
type RemoteListenerOptions struct {
	ListenAddress string
	AdvertiseURL  string
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
	advertiseURL, err := remoteAdvertiseURL(configuredOptions.AdvertiseURL, listener.Addr())
	if err != nil {
		_ = listener.Close()
		return err
	}
	ca, err := newCertificateAuthority()
	if err != nil {
		_ = listener.Close()
		return err
	}
	serverCertificate, err := ca.issueServer(advertiseURL.Hostname())
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
		Handler:           remoteGatewayHandler{gateway: g, authority: advertiseURL.Host},
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
	g.remoteURL = advertiseURL.String()
	g.remoteCA = ca
	g.remoteOptions = configuredOptions
	g.mu.Unlock()
	go func() { _ = server.Serve(tlsListener) }()
	return nil
}

func normalizedRemoteListenerOptions(options RemoteListenerOptions) RemoteListenerOptions {
	return RemoteListenerOptions{
		ListenAddress: strings.TrimSpace(options.ListenAddress),
		AdvertiseURL:  strings.TrimSpace(options.AdvertiseURL),
	}
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
	if g.remoteOptions != normalizedRemoteListenerOptions(options) {
		return true, errors.New("remote credential gateway configuration changed; restart the daemon to apply listener coordinates")
	}
	return true, nil
}

func (g *Gateway) RemoteURL() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.remoteURL
}

type remoteGatewayHandler struct {
	gateway   *Gateway
	authority string
}

func (h remoteGatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, routed := proxyRoute(r.URL.EscapedPath())
	if !routed || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	capability := strings.TrimSpace(r.Header.Get(CapabilityHeader))
	if !validCapabilitySyntax(capability) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		h.gateway.writeLog(r.Method, "", http.StatusUnauthorized, "")
		return
	}
	h.gateway.serveProxyRequest(w, r, proxyRequestAccess{
		route: route, capability: capability, suffixRoute: route,
		authority: h.authority, remote: true,
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

func remoteAdvertiseURL(raw string, address net.Addr) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://" + address.String()
	}
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

func (ca *certificateAuthority) issueServer(host string) (tls.Certificate, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate remote credential gateway server key: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: newCertificateSerial(), Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 1, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
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
