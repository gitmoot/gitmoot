package credgw

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const networkRenewPrefix = "/_gitmoot/renew/"

// NetworkProxyConfig contains the host-owned TLS material for a proxy-only
// listener. ClientCAKey never leaves the host; it signs one client certificate
// for each network proxy lease.
type NetworkProxyConfig struct {
	Address           string
	ServerCertificate tls.Certificate
	ClientCA          *x509.Certificate
	ClientCAKey       crypto.Signer
}

// NetworkProxy is a mandatory-mTLS transport over a Gateway's request-time
// proxy entries. It never dispatches the legacy entries that retain credentials.
type NetworkProxy struct {
	gateway  *Gateway
	listener net.Listener
	server   *http.Server
	clientCA *x509.Certificate
	caKey    crypto.Signer
}

// NetworkLease carries no provider credential. Its client certificate and
// rotating capability authorize one route until the absolute deadline.
type NetworkLease struct {
	lease       *Lease
	network     *NetworkProxy
	certificate tls.Certificate
	deadline    time.Time

	mu         sync.RWMutex
	renewMu    sync.Mutex
	capability string
}

func (g *Gateway) StartNetworkProxy(config NetworkProxyConfig) (*NetworkProxy, error) {
	if g == nil {
		return nil, errors.New("credential gateway is not running")
	}
	if strings.TrimSpace(config.Address) == "" {
		return nil, errors.New("network credential gateway listen address is required")
	}
	if len(config.ServerCertificate.Certificate) == 0 || config.ServerCertificate.PrivateKey == nil {
		return nil, errors.New("network credential gateway server TLS certificate is required")
	}
	if err := validateClientCA(config.ClientCA, config.ClientCAKey); err != nil {
		return nil, err
	}

	g.mu.RLock()
	closed := g.closed
	g.mu.RUnlock()
	if closed {
		return nil, errors.New("credential gateway is not running")
	}

	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(config.ClientCA)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{config.ServerCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
		Time:         g.nowTime,
	}
	listener, err := net.Listen("tcp", config.Address)
	if err != nil {
		return nil, fmt.Errorf("listen for network credential gateway: %w", err)
	}
	network := &NetworkProxy{
		gateway: g, listener: listener, clientCA: config.ClientCA, caKey: config.ClientCAKey,
	}
	network.server = &http.Server{
		Handler:           network,
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = listener.Close()
		return nil, errors.New("credential gateway is not running")
	}
	g.networks[network] = struct{}{}
	g.mu.Unlock()
	go func() {
		_ = network.server.Serve(tls.NewListener(listener, tlsConfig))
	}()
	return network, nil
}

func (n *NetworkProxy) URL() string {
	if n == nil || n.listener == nil {
		return ""
	}
	return "https://" + n.listener.Addr().String()
}

func (n *NetworkProxy) Close(ctx context.Context) error {
	if n == nil || n.server == nil {
		return nil
	}
	err := n.server.Shutdown(ctx)
	if n.gateway != nil {
		n.gateway.mu.Lock()
		delete(n.gateway.networks, n)
		n.gateway.mu.Unlock()
	}
	return err
}

func (n *NetworkProxy) RegisterProxy(jobID string, deadline time.Time, policy ProxyPolicy, resolver CredentialResolver) (*NetworkLease, error) {
	if n == nil || n.gateway == nil {
		return nil, errors.New("network credential gateway is not running")
	}
	now := n.gateway.nowTime()
	deadline = deadline.UTC()
	if !deadline.After(now) {
		return nil, errors.New("network credential gateway deadline must be in the future")
	}
	certificate, certificateHash, err := issueNetworkClientCertificate(n.clientCA, n.caKey, jobID, now, deadline)
	if err != nil {
		return nil, err
	}
	lease, err := n.gateway.RegisterProxy(jobID, policy, resolver)
	if err != nil {
		return nil, err
	}

	n.gateway.mu.Lock()
	registered, ok := n.gateway.proxyEntries[lease.route]
	if !ok || registered.placeholder != lease.placeholder {
		n.gateway.mu.Unlock()
		lease.Revoke()
		return nil, errors.New("network credential gateway lease registration disappeared")
	}
	registered.networkEnabled = true
	registered.clientCertHash = certificateHash
	registered.absoluteDeadline = deadline
	registered.capability.expiresAt = earliestTime(registered.capability.expiresAt, deadline)
	n.gateway.proxyEntries[lease.route] = registered
	n.gateway.mu.Unlock()

	return &NetworkLease{
		lease: lease, network: n, certificate: certificate, deadline: deadline,
		capability: lease.capability,
	}, nil
}

func (l *NetworkLease) URL() string {
	if l == nil || l.network == nil || l.lease == nil {
		return ""
	}
	return l.network.URL() + l.lease.route
}

func (l *NetworkLease) Placeholder() string {
	if l == nil || l.lease == nil {
		return ""
	}
	return l.lease.Placeholder()
}

func (l *NetworkLease) Capability() string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.capability
}

func (l *NetworkLease) ClientCertificate() tls.Certificate {
	if l == nil {
		return tls.Certificate{}
	}
	return l.certificate
}

func (l *NetworkLease) Deadline() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.deadline
}

func (l *NetworkLease) Revoke() {
	if l != nil && l.lease != nil {
		l.lease.Revoke()
	}
}

// Renew rotates the capability over the authenticated network transport. The
// route URL remains stable; callers read Capability again for later requests.
func (l *NetworkLease) Renew(ctx context.Context, client *http.Client) error {
	if l == nil || l.network == nil || l.lease == nil || client == nil {
		return errors.New("network credential gateway renewal requires a lease and client")
	}
	l.renewMu.Lock()
	defer l.renewMu.Unlock()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.network.URL()+networkRenewPrefix+strings.TrimPrefix(l.lease.route, "/_gitmoot/proxy/"), nil)
	if err != nil {
		return err
	}
	request.Header.Set(CapabilityHeader, l.Capability())
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("renew network credential gateway capability: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("renew network credential gateway capability: status %d", response.StatusCode)
	}
	capability, err := io.ReadAll(io.LimitReader(response.Body, proxyCapabilityBytes*2+1))
	if err != nil {
		return fmt.Errorf("read renewed network credential gateway capability: %w", err)
	}
	value := strings.TrimSpace(string(capability))
	if !validCapabilityEncoding(value) {
		return errors.New("renew network credential gateway capability: malformed response")
	}
	l.mu.Lock()
	l.capability = value
	l.mu.Unlock()
	return nil
}

func (n *NetworkProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	certificateHash, ok := requestClientCertificateHash(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if route, ok := networkRenewRoute(r.URL.EscapedPath()); ok {
		n.serveRenewal(w, r, route, certificateHash)
		return
	}
	if route, ok := proxyRoute(r.URL.EscapedPath()); ok {
		capability := strings.TrimSpace(r.Header.Get(CapabilityHeader))
		if !validCapabilityEncoding(capability) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		n.gateway.serveProxyRequest(w, r, proxyRequestAccess{
			route: route, capability: capability, suffixRoute: route,
			authority: n.listener.Addr().String(), clientCertHash: certificateHash, network: true,
		})
		return
	}
	http.NotFound(w, r)
}

func (n *NetworkProxy) serveRenewal(w http.ResponseWriter, r *http.Request, route string, certificateHash [sha256.Size]byte) {
	if r.Method != http.MethodPost || r.URL.IsAbs() || r.URL.Host != "" || !strings.EqualFold(r.Host, n.listener.Addr().String()) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	capability := strings.TrimSpace(r.Header.Get(CapabilityHeader))
	if !validCapabilityEncoding(capability) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	renewed, err := n.gateway.renewProxyCapability(route, capability, certificateHash)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, renewed)
}

func (g *Gateway) renewProxyCapability(route, capability string, certificateHash [sha256.Size]byte) (string, error) {
	newCapability, newHash, err := mintProxyCapability()
	if err != nil {
		return "", err
	}
	now := g.nowTime()
	g.mu.Lock()
	defer g.mu.Unlock()
	registered, ok := g.proxyEntries[route]
	if !ok || !registered.networkEnabled || subtle.ConstantTimeCompare(certificateHash[:], registered.clientCertHash[:]) != 1 {
		return "", errors.New("network credential gateway lease is unavailable")
	}
	if registered.absoluteDeadline.IsZero() || !now.Before(registered.absoluteDeadline) || !validProxyCapability(capability, registered, now) {
		return "", errors.New("network credential gateway lease is expired")
	}
	previous := registered.capability
	previous.expiresAt = earliestTime(previous.expiresAt, now.Add(proxyCapabilityOverlap), registered.absoluteDeadline)
	registered.previous = previous
	registered.capability = proxyCapability{
		hash: newHash, expiresAt: earliestTime(now.Add(proxyCapabilityTTL), registered.absoluteDeadline),
	}
	g.proxyEntries[route] = registered
	return newCapability, nil
}

func requestClientCertificateHash(r *http.Request) ([sha256.Size]byte, bool) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(r.TLS.PeerCertificates[0].Raw), true
}

func networkRenewRoute(escapedPath string) (string, bool) {
	if !strings.HasPrefix(escapedPath, networkRenewPrefix) {
		return "", false
	}
	segment, remainder, _ := strings.Cut(strings.TrimPrefix(escapedPath, networkRenewPrefix), "/")
	if len(segment) != 32 || remainder != "" {
		return "", false
	}
	return "/_gitmoot/proxy/" + segment, true
}

func validCapabilityEncoding(capability string) bool {
	if len(capability) != proxyCapabilityBytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(capability)
	return err == nil && len(decoded) == proxyCapabilityBytes
}

func validateClientCA(certificate *x509.Certificate, key crypto.Signer) error {
	if certificate == nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("network credential gateway client CA certificate is required")
	}
	if key == nil {
		return errors.New("network credential gateway client CA signing key is required")
	}
	want, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return fmt.Errorf("marshal network credential gateway client CA public key: %w", err)
	}
	got, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return fmt.Errorf("marshal network credential gateway client CA signing key: %w", err)
	}
	if !bytes.Equal(want, got) {
		return errors.New("network credential gateway client CA certificate and signing key do not match")
	}
	return nil
}

func issueNetworkClientCertificate(ca *x509.Certificate, caKey crypto.Signer, jobID string, now, deadline time.Time) (tls.Certificate, [sha256.Size]byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, [sha256.Size]byte{}, fmt.Errorf("generate network credential gateway client key: %w", err)
	}
	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return tls.Certificate{}, [sha256.Size]byte{}, fmt.Errorf("generate network credential gateway client serial: %w", err)
	}
	notAfter := earliestTime(deadline, ca.NotAfter)
	if !notAfter.After(now) {
		return tls.Certificate{}, [sha256.Size]byte{}, errors.New("network credential gateway client certificate deadline is not usable")
	}
	template := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(serialBytes),
		Subject:      pkix.Name{CommonName: "gitmoot-job:" + jobID, Organization: []string{"gitmoot"}},
		NotBefore:    now.Add(-time.Minute), NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &privateKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, [sha256.Size]byte{}, fmt.Errorf("issue network credential gateway client certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, [sha256.Size]byte{}, fmt.Errorf("parse network credential gateway client certificate: %w", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: privateKey, Leaf: leaf}
	return certificate, sha256.Sum256(der), nil
}

func earliestTime(values ...time.Time) time.Time {
	var earliest time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if earliest.IsZero() || value.Before(earliest) {
			earliest = value
		}
	}
	return earliest
}
