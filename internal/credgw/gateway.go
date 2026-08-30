package credgw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultAnthropicUpstream = "https://api.anthropic.com"

const CapabilityHeader = "X-Gitmoot-Capability"

const (
	proxyCapabilityBytes = 32
	proxyCapabilityTTL   = 5 * time.Minute
)

type CredentialKind string

const (
	CredentialBearer CredentialKind = "bearer"
	CredentialAPIKey CredentialKind = "api_key"
)

type Credential struct {
	Kind  CredentialKind
	Value string
}

type ProxyAuthKind string

const (
	ProxyAuthBearer ProxyAuthKind = "bearer"
	ProxyAuthHeader ProxyAuthKind = "header"
	// ProxyAuthResolved defers bearer-versus-header placement until the host
	// resolver loads the current credential. It is valid only for a
	// sandbox-bound mTLS lease, after every sandbox-side credential has already
	// been authenticated.
	ProxyAuthResolved ProxyAuthKind = "resolved"
)

// ProxyPolicy pins one generic lease to a complete origin, normalized base
// path, and credential placement. AllowLoopbackHTTP exists solely for tests;
// production callers leave it false and therefore require HTTPS.
type ProxyPolicy struct {
	Upstream          string
	AuthKind          ProxyAuthKind
	Header            string
	AllowLoopbackHTTP bool
	// SandboxID and Runtime are an indivisible remote-lease identity. When set,
	// the capability and client certificate are bound to both values and expire
	// at ExpiresAt. Local loopback leases leave all four fields empty.
	SandboxID    string
	Runtime      string
	ExpiresAt    time.Time
	AllowedHosts []string
}

// ResolvedCredential is returned for every proxied request. The gateway checks
// that the current metadata still matches the lease's pinned policy before it
// uses Value, so grant/config changes fail closed and key rotation is immediate.
type ResolvedCredential struct {
	Value    string
	Upstream string
	AuthKind ProxyAuthKind
	Header   string
}

type CredentialResolver func(context.Context) (ResolvedCredential, error)

// Policy is snapshotted when a job lease is created. Upstream is fixed by the
// host; AllowedHosts is an exact hostname allowlist, never child-controlled.
type Policy struct {
	Upstream     string
	AllowedHosts []string
}

type LogFunc func(format string, args ...any)

var (
	DefaultRegistry         = NewRegistry()
	DefaultLogf     LogFunc = log.Printf
)

type Gateway struct {
	listener       net.Listener
	server         *http.Server
	remoteListener net.Listener
	remoteServer   *http.Server
	remoteURL      string
	remoteCA       *certificateAuthority
	remoteOptions  RemoteListenerOptions
	client         *http.Client
	logf           LogFunc

	mu           sync.RWMutex
	entries      map[string]entry
	proxyEntries map[string]proxyEntry
	now          func() time.Time
	closed       bool
}

type entry struct {
	jobID      string
	credential Credential
	upstream   *url.URL
	allowed    map[string]struct{}
}

type proxyEntry struct {
	jobID             string
	placeholder       string
	policy            ProxyPolicy
	upstream          *url.URL
	resolver          CredentialResolver
	capability        proxyCapability
	clientCertificate [sha256.Size]byte
}

type proxyCapability struct {
	hash      [sha256.Size]byte
	expiresAt time.Time
}

func Start(logf LogFunc) (*Gateway, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for model gateway: %w", err)
	}
	gateway := &Gateway{
		listener: listener,
		client: &http.Client{
			Transport: proxyHTTPTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("model gateway upstream redirects are disabled")
			},
		},
		logf:         logf,
		entries:      make(map[string]entry),
		proxyEntries: make(map[string]proxyEntry),
		now:          time.Now,
	}
	gateway.server = &http.Server{
		Handler:           gateway,
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		_ = gateway.server.Serve(listener)
	}()
	return gateway, nil
}

func proxyHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Go drops forbidden HTTP/2 trailers before exposing Response.Trailer. Keep
	// the upstream protocol on HTTP/1.1 so response metadata is not silently
	// protocol-dependent; an unexpected HTTP/2 response is refused before its
	// body is streamed.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	return transport
}

// RegisterProxy creates a route-scoped lease without loading credential bytes.
// The resolver is called for every request and is the only source of the real
// value, current grant, mode, and configuration.
func (g *Gateway) RegisterProxy(jobID string, policy ProxyPolicy, resolver CredentialResolver) (*Lease, error) {
	if g == nil {
		return nil, errors.New("credential gateway is not running")
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("credential gateway job id is required")
	}
	if resolver == nil {
		return nil, errors.New("credential gateway resolver is required")
	}
	validated, upstream, err := ValidateProxyPolicy(policy)
	if err != nil {
		return nil, err
	}
	placeholder, err := mintPlaceholder(jobID)
	if err != nil {
		return nil, err
	}
	route, err := mintProxyRoute()
	if err != nil {
		return nil, err
	}
	capability, capabilityHash, err := mintProxyCapability()
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, errors.New("credential gateway is not running")
	}
	now := g.nowTime()
	expiresAt := now.Add(proxyCapabilityTTL)
	var remoteMaterial RemoteMaterial
	var clientCertificate [sha256.Size]byte
	if validated.SandboxID != "" {
		if g.remoteCA == nil || g.remoteServer == nil || g.remoteURL == "" {
			return nil, errors.New("remote credential gateway is not configured")
		}
		expiresAt = validated.ExpiresAt
		remoteMaterial, clientCertificate, err = g.remoteCA.issueClient(validated.SandboxID, validated.Runtime, expiresAt)
		if err != nil {
			return nil, err
		}
		remoteMaterial.URL = g.remoteURL + route
		remoteMaterial.Capability = capability
		remoteMaterial.Placeholder = placeholder
	}
	g.proxyEntries[route] = proxyEntry{
		jobID: jobID, placeholder: placeholder, policy: validated,
		upstream: upstream, resolver: resolver,
		capability:        proxyCapability{hash: capabilityHash, expiresAt: expiresAt},
		clientCertificate: clientCertificate,
	}
	return &Lease{gateway: g, placeholder: placeholder, route: route, capability: capability, remote: remoteMaterial}, nil
}

func (g *Gateway) URL() string {
	if g == nil || g.listener == nil {
		return ""
	}
	return "http://" + g.listener.Addr().String()
}

func (g *Gateway) Register(jobID string, credential Credential, policy Policy) (string, error) {
	if g == nil {
		return "", errors.New("model gateway is not running")
	}
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("model gateway job id is required")
	}
	if strings.TrimSpace(credential.Value) == "" {
		return "", errors.New("model gateway credential is empty")
	}
	if credential.Kind != CredentialBearer && credential.Kind != CredentialAPIKey {
		return "", fmt.Errorf("unsupported model gateway credential kind %q", credential.Kind)
	}
	upstream, allowed, err := validatePolicy(policy)
	if err != nil {
		return "", err
	}
	placeholder, err := mintPlaceholder(jobID)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return "", errors.New("model gateway is not running")
	}
	g.entries[placeholder] = entry{
		jobID:      jobID,
		credential: credential,
		upstream:   upstream,
		allowed:    allowed,
	}
	return placeholder, nil
}

func (g *Gateway) Revoke(placeholder string) {
	if g == nil || placeholder == "" {
		return
	}
	g.mu.Lock()
	delete(g.entries, placeholder)
	g.mu.Unlock()
}

func (g *Gateway) revokeProxy(route, placeholder string) {
	if g == nil || route == "" || placeholder == "" {
		return
	}
	g.mu.Lock()
	if registered, ok := g.proxyEntries[route]; ok && registered.placeholder == placeholder {
		delete(g.proxyEntries, route)
	}
	g.mu.Unlock()
}

// RevokeSandbox removes every in-process capability bound to sandboxID. It is
// idempotent and is used by both normal teardown and provider startup reap.
func (g *Gateway) RevokeSandbox(sandboxID string) {
	if g == nil || strings.TrimSpace(sandboxID) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for route, registered := range g.proxyEntries {
		if registered.policy.SandboxID == sandboxID {
			delete(g.proxyEntries, route)
		}
	}
}

func (g *Gateway) Close(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	clear(g.entries)
	clear(g.proxyEntries)
	g.mu.Unlock()
	var errs []error
	if err := g.server.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if g.remoteServer != nil {
		if err := g.remoteServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	g.client.CloseIdleConnections()
	return errors.Join(errs...)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route, routed := proxyRoute(r.URL.EscapedPath()); routed {
		g.serveProxy(w, r, route)
		return
	}
	placeholder := requestPlaceholder(r)
	g.mu.RLock()
	registered, ok := g.entries[placeholder]
	g.mu.RUnlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, "", http.StatusUnauthorized, "")
		return
	}
	if _, ok := registered.allowed[strings.ToLower(registered.upstream.Hostname())]; !ok {
		http.Error(w, "upstream refused", http.StatusBadGateway)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadGateway, registered.jobID)
		return
	}

	upstreamURL := *registered.upstream
	upstreamURL.Path = joinURLPath(registered.upstream.Path, r.URL.Path)
	upstreamURL.RawPath = ""
	upstreamURL.RawQuery = r.URL.RawQuery
	outbound := r.Clone(r.Context())
	outbound.URL = &upstreamURL
	outbound.Host = registered.upstream.Host
	outbound.RequestURI = ""
	outbound.Header = r.Header.Clone()
	removeHopHeaders(outbound.Header)
	outbound.Header.Del("Authorization")
	outbound.Header.Del("X-Api-Key")
	outbound.Header.Del("Proxy-Authorization")
	switch registered.credential.Kind {
	case CredentialAPIKey:
		outbound.Header.Set("X-Api-Key", registered.credential.Value)
	case CredentialBearer:
		outbound.Header.Set("Authorization", "Bearer "+registered.credential.Value)
	}

	response, err := g.client.Do(outbound)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadGateway, registered.jobID)
		return
	}
	defer response.Body.Close()
	removeHopHeaders(response.Header)
	copyHeader(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	streamResponse(w, response.Body)
	g.writeLog(r.Method, registered.upstream.Hostname(), response.StatusCode, registered.jobID)
}

func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request, route string) {
	capability, ok := proxyRequestCapability(r.URL.EscapedPath(), route)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, "", http.StatusUnauthorized, "")
		return
	}
	g.serveProxyRequest(w, r, proxyRequestAccess{
		route: route, capability: capability, suffixRoute: route + "/" + capability,
		authority: g.listener.Addr().String(),
	})
}

type proxyRequestAccess struct {
	route             string
	capability        string
	suffixRoute       string
	authority         string
	clientCertificate [sha256.Size]byte
	remote            bool
}

func (g *Gateway) serveProxyRequest(w http.ResponseWriter, r *http.Request, access proxyRequestAccess) {
	g.mu.RLock()
	registered, ok := g.proxyEntries[access.route]
	g.mu.RUnlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, "", http.StatusUnauthorized, "")
		return
	}
	if proxyRequestPlaceholder(r, registered.policy) != registered.placeholder {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusUnauthorized, registered.jobID)
		return
	}
	if r.URL.IsAbs() || r.URL.Host != "" || !strings.EqualFold(r.Host, access.authority) {
		http.Error(w, "request target refused", http.StatusBadRequest)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadRequest, registered.jobID)
		return
	}
	suffix, err := proxyRequestSuffix(r.URL.EscapedPath(), access.suffixRoute)
	if err != nil {
		http.Error(w, "request path refused", http.StatusBadRequest)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadRequest, registered.jobID)
		return
	}
	if !validProxyCapability(access.capability, registered, g.nowTime()) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusUnauthorized, registered.jobID)
		return
	}
	if registered.policy.SandboxID != "" {
		if !access.remote || subtle.ConstantTimeCompare(access.clientCertificate[:], registered.clientCertificate[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusUnauthorized, registered.jobID)
			return
		}
	}
	resolved, err := registered.resolver(r.Context())
	if err != nil || strings.TrimSpace(resolved.Value) == "" || !resolvedMatchesProxyPolicy(resolved, registered.policy) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusUnauthorized, registered.jobID)
		return
	}

	upstreamURL := *registered.upstream
	upstreamURL.Path = joinProxyPath(registered.upstream.Path, suffix)
	upstreamURL.RawPath = ""
	upstreamURL.RawQuery = r.URL.RawQuery
	outbound := r.Clone(r.Context())
	outbound.URL = &upstreamURL
	outbound.Host = registered.upstream.Host
	outbound.RequestURI = ""
	outbound.Header = r.Header.Clone()
	removeHopHeaders(outbound.Header)
	removeCredentialHeaders(outbound.Header, registered.policy.Header)
	outbound.Header.Del(CapabilityHeader)
	// Let the host transport negotiate and transparently decode gzip. A
	// sandbox-supplied Accept-Encoding would otherwise leave compressed bytes
	// opaque to the credential redactor and recoverable after forwarding.
	outbound.Header.Del("Accept-Encoding")
	switch resolved.AuthKind {
	case ProxyAuthBearer:
		outbound.Header.Set("Authorization", "Bearer "+resolved.Value)
	case ProxyAuthHeader:
		outbound.Header.Set(resolved.Header, resolved.Value)
	}

	response, err := g.client.Do(outbound)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadGateway, registered.jobID)
		return
	}
	defer response.Body.Close()
	if !responseSafeToStream(response) {
		http.Error(w, "upstream response failed", http.StatusBadGateway)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadGateway, registered.jobID)
		return
	}
	removeHopHeaders(response.Header)
	copyRedactedHeader(w.Header(), response.Header, resolved.Value)
	w.WriteHeader(response.StatusCode)
	_ = streamRedactedResponse(w, response.Body, resolved.Value)
	g.writeLog(r.Method, registered.upstream.Hostname(), response.StatusCode, registered.jobID)
}

func responseSafeToStream(response *http.Response) bool {
	// This decision intentionally uses only the initial response metadata. HTTP
	// trailers are complete only at EOF; waiting for them would reintroduce
	// whole-response staging and break unbounded streams such as SSE.
	return response != nil &&
		response.ProtoMajor == 1 &&
		!headerContains(response.Header, "Content-Encoding")
}

func headerContains(header http.Header, name string) bool {
	for candidate := range header {
		if strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

// ValidateProxyPolicy returns a canonical policy and parsed upstream. Callers
// persist the canonical Upstream so later request-time comparisons are exact.
func ValidateProxyPolicy(policy ProxyPolicy) (ProxyPolicy, *url.URL, error) {
	raw := strings.TrimSpace(policy.Upstream)
	upstream, err := url.Parse(raw)
	if err != nil || raw == "" || !upstream.IsAbs() || upstream.Hostname() == "" || upstream.Opaque != "" {
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: require an absolute URL with a host", raw)
	}
	if upstream.User != nil {
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: userinfo is not allowed", raw)
	}
	if upstream.RawQuery != "" || upstream.ForceQuery {
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: query is not allowed", raw)
	}
	if upstream.Fragment != "" || strings.Contains(raw, "#") {
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: fragment is not allowed", raw)
	}
	upstream.Scheme = strings.ToLower(upstream.Scheme)
	if upstream.Scheme != "https" {
		if upstream.Scheme != "http" || !policy.AllowLoopbackHTTP || !isLoopbackHost(upstream.Hostname()) {
			return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: HTTPS is required", raw)
		}
	}
	basePath, err := normalizedProxyPath(upstream.EscapedPath())
	if err != nil {
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy upstream %q: %w", raw, err)
	}
	upstream.Path = basePath
	upstream.RawPath = ""
	upstream.RawQuery = ""
	upstream.Fragment = ""

	validated := ProxyPolicy{
		Upstream: upstream.String(), AuthKind: policy.AuthKind,
		AllowLoopbackHTTP: policy.AllowLoopbackHTTP,
		SandboxID:         strings.TrimSpace(policy.SandboxID), Runtime: strings.TrimSpace(policy.Runtime),
		ExpiresAt: policy.ExpiresAt,
	}
	remote := validated.SandboxID != "" || validated.Runtime != "" || !validated.ExpiresAt.IsZero() || len(policy.AllowedHosts) > 0
	if remote {
		if validated.SandboxID == "" || validated.Runtime == "" || validated.ExpiresAt.IsZero() {
			return ProxyPolicy{}, nil, errors.New("remote proxy policy requires sandbox id, runtime, and expiry")
		}
		if !validated.ExpiresAt.After(time.Now()) {
			return ProxyPolicy{}, nil, errors.New("remote proxy policy expiry must be in the future")
		}
		allowed, err := normalizeAllowedHosts(policy.AllowedHosts)
		if err != nil {
			return ProxyPolicy{}, nil, err
		}
		if _, ok := allowed[strings.ToLower(upstream.Hostname())]; !ok {
			return ProxyPolicy{}, nil, fmt.Errorf("proxy upstream host %q is not allowlisted", upstream.Hostname())
		}
		validated.AllowedHosts = sortedHostSet(allowed)
	}
	switch policy.AuthKind {
	case ProxyAuthBearer:
		if strings.TrimSpace(policy.Header) != "" {
			return ProxyPolicy{}, nil, errors.New("bearer proxy auth cannot set a header name")
		}
	case ProxyAuthHeader:
		header := http.CanonicalHeaderKey(strings.TrimSpace(policy.Header))
		if !validHTTPToken(header) {
			return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy header %q: must be an HTTP token", policy.Header)
		}
		if forbiddenProxyHeader(header) {
			return ProxyPolicy{}, nil, fmt.Errorf("proxy header %q is not allowed", policy.Header)
		}
		validated.Header = header
	case ProxyAuthResolved:
		if !remote {
			return ProxyPolicy{}, nil, errors.New("resolved proxy auth requires a sandbox-bound remote policy")
		}
		if strings.TrimSpace(policy.Header) != "" {
			return ProxyPolicy{}, nil, errors.New("resolved proxy auth cannot set a header name")
		}
	default:
		return ProxyPolicy{}, nil, fmt.Errorf("invalid proxy auth kind %q", policy.AuthKind)
	}
	return validated, upstream, nil
}

func resolvedMatchesProxyPolicy(resolved ResolvedCredential, want ProxyPolicy) bool {
	current, _, err := ValidateProxyPolicy(ProxyPolicy{
		Upstream: resolved.Upstream, AuthKind: resolved.AuthKind, Header: resolved.Header,
		AllowLoopbackHTTP: want.AllowLoopbackHTTP,
	})
	if err != nil || current.Upstream != want.Upstream {
		return false
	}
	if want.AuthKind == ProxyAuthResolved {
		return current.AuthKind == ProxyAuthBearer || current.AuthKind == ProxyAuthHeader
	}
	return current.AuthKind == want.AuthKind && current.Header == want.Header
}

func normalizeAllowedHosts(hosts []string) (map[string]struct{}, error) {
	if len(hosts) == 0 {
		return nil, errors.New("remote proxy policy upstream allowlist is required")
	}
	allowed := make(map[string]struct{}, len(hosts))
	for _, value := range hosts {
		host := strings.ToLower(strings.TrimSpace(value))
		if host == "" || strings.ContainsAny(host, "/:@?#\\") {
			return nil, fmt.Errorf("invalid proxy allowlist host %q", value)
		}
		allowed[host] = struct{}{}
	}
	return allowed, nil
}

func sortedHostSet(hosts map[string]struct{}) []string {
	values := make([]string, 0, len(hosts))
	for host := range hosts {
		values = append(values, host)
	}
	sort.Strings(values)
	return values
}

// For an authenticated RegisterProxy lease pinned to an operator-selected
// HTTPS upstream, prevent a non-malicious upstream's accidental exact-byte
// reflection of the credential in response body bytes or HTTP field
// names/values from reaching the sandbox. Malicious or compromised upstreams
// and transformed application payloads are out of scope. The finite reversible
// forms below are defense in depth, not an application-layer DLP guarantee;
// that exclusion includes application/gzip data without Content-Encoding.
func copyRedactedHeader(destination, source http.Header, secret string) {
	patterns := credentialRedactionPatterns(secret)
	destination.Del("Content-Length")
	for name, values := range source {
		if strings.EqualFold(name, "Content-Length") || redactCredentialText(name, patterns) != name {
			continue
		}
		for _, value := range values {
			destination.Add(name, redactCredentialText(value, patterns))
		}
	}
}

func streamRedactedResponse(destination http.ResponseWriter, source io.Reader, secret string) error {
	patterns := credentialRedactionPatterns(secret)
	buffered := bufio.NewWriter(destination)
	redactor := &credentialRedactingWriter{destination: buffered, patterns: patterns}
	controller := http.NewResponseController(destination)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := redactor.Write(buffer[:n]); err != nil {
				return err
			}
			if err := buffered.Flush(); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}
	if err := redactor.flush(); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

type credentialRedactionPattern struct {
	value     []byte
	asciiFold bool
}

// credentialRedactionPatterns derives standard reversible textual forms from
// the host credential. The proxy applies this one set to headers and streaming
// bodies so an upstream cannot evade the boundary merely by changing transport
// encoding or ASCII case.
func credentialRedactionPatterns(secret string) []credentialRedactionPattern {
	if secret == "" {
		return nil
	}
	raw := []byte(secret)
	patterns := make(map[string]bool)
	add := func(value string, asciiFold bool) {
		if value == "" {
			return
		}
		patterns[value] = patterns[value] || asciiFold
	}
	add(secret, true)
	add(url.QueryEscape(secret), true)
	add(url.PathEscape(secret), true)
	add(percentEncodeAll(raw), true)
	add(hex.EncodeToString(raw), true)
	add(base64.StdEncoding.EncodeToString(raw), false)
	add(base64.RawStdEncoding.EncodeToString(raw), false)
	add(base64.URLEncoding.EncodeToString(raw), false)
	add(base64.RawURLEncoding.EncodeToString(raw), false)
	add(base32.StdEncoding.EncodeToString(raw), true)
	add(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), true)
	add(base32.HexEncoding.EncodeToString(raw), true)
	add(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), true)
	quoted := strconv.Quote(secret)
	add(quoted[1:len(quoted)-1], false)

	result := make([]credentialRedactionPattern, 0, len(patterns))
	for value, asciiFold := range patterns {
		result = append(result, credentialRedactionPattern{value: []byte(value), asciiFold: asciiFold})
	}
	sort.Slice(result, func(i, j int) bool { return len(result[i].value) > len(result[j].value) })
	return result
}

func percentEncodeAll(value []byte) string {
	const digits = "0123456789ABCDEF"
	encoded := make([]byte, 0, len(value)*3)
	for _, character := range value {
		encoded = append(encoded, '%', digits[character>>4], digits[character&0x0f])
	}
	return string(encoded)
}

func redactCredentialText(value string, patterns []credentialRedactionPattern) string {
	var redacted strings.Builder
	writer := &credentialRedactingWriter{destination: &redacted, patterns: patterns}
	_, _ = writer.Write([]byte(value))
	_ = writer.flush()
	return redacted.String()
}

type credentialRedactingWriter struct {
	destination io.Writer
	patterns    []credentialRedactionPattern
	pending     []byte
}

func (w *credentialRedactingWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		w.pending = append(w.pending, value)
		for len(w.pending) > 0 {
			matchLength, couldGrow := w.match()
			if matchLength > 0 && !couldGrow {
				if _, err := io.WriteString(w.destination, "[REDACTED]"); err != nil {
					return 0, err
				}
				w.pending = w.pending[matchLength:]
				continue
			}
			if couldGrow {
				break
			}
			if _, err := w.destination.Write(w.pending[:1]); err != nil {
				return 0, err
			}
			w.pending = w.pending[1:]
		}
	}
	return len(data), nil
}

func (w *credentialRedactingWriter) match() (matchLength int, couldGrow bool) {
	for _, pattern := range w.patterns {
		if patternPrefix(pattern, w.pending) && len(w.pending) < len(pattern.value) {
			couldGrow = true
		}
		if len(w.pending) >= len(pattern.value) && patternPrefix(pattern, w.pending[:len(pattern.value)]) && len(pattern.value) > matchLength {
			matchLength = len(pattern.value)
		}
	}
	return matchLength, couldGrow
}

func patternPrefix(pattern credentialRedactionPattern, prefix []byte) bool {
	if len(prefix) > len(pattern.value) {
		return false
	}
	want := pattern.value[:len(prefix)]
	if pattern.asciiFold {
		return bytes.EqualFold(want, prefix)
	}
	return bytes.Equal(want, prefix)
}

func (w *credentialRedactingWriter) flush() error {
	for len(w.pending) > 0 {
		matchLength, _ := w.match()
		if matchLength > 0 {
			if _, err := io.WriteString(w.destination, "[REDACTED]"); err != nil {
				return err
			}
			w.pending = w.pending[matchLength:]
			continue
		}
		if _, err := w.destination.Write(w.pending[:1]); err != nil {
			return err
		}
		w.pending = w.pending[1:]
	}
	return nil
}

func normalizedProxyPath(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || strings.Contains(decoded, `\`) || hasDotPathSegment(decoded) {
		return "", errors.New("base path contains an invalid or escaping segment")
	}
	if containsEncodedSlashOrBackslash(escaped) {
		return "", errors.New("base path contains an encoded path separator")
	}
	normalized := path.Clean("/" + strings.TrimLeft(decoded, "/"))
	return normalized, nil
}

func proxyRequestSuffix(escapedPath, route string) (string, error) {
	raw := strings.TrimPrefix(escapedPath, route)
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "/") || containsEncodedSlashOrBackslash(raw) {
		return "", errors.New("invalid proxy path")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(decoded, `\`) || hasDotPathSegment(decoded) {
		return "", errors.New("proxy path escapes the configured base path")
	}
	return decoded, nil
}

func proxyRequestCapability(escapedPath, route string) (string, bool) {
	raw := strings.TrimPrefix(escapedPath, route)
	if !strings.HasPrefix(raw, "/") {
		return "", false
	}
	capability, _, _ := strings.Cut(strings.TrimPrefix(raw, "/"), "/")
	if len(capability) != proxyCapabilityBytes*2 {
		return "", false
	}
	decoded, err := hex.DecodeString(capability)
	return capability, err == nil && len(decoded) == proxyCapabilityBytes
}

func validProxyCapability(capability string, registered proxyEntry, now time.Time) bool {
	decoded, err := hex.DecodeString(capability)
	if err != nil || len(decoded) != proxyCapabilityBytes {
		return false
	}
	presented := sha256.Sum256(decoded)
	return subtle.ConstantTimeCompare(presented[:], registered.capability.hash[:]) == 1 && now.Before(registered.capability.expiresAt)
}

func (g *Gateway) nowTime() time.Time {
	if g != nil && g.now != nil {
		return g.now()
	}
	return time.Now()
}

func hasDotPathSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func containsEncodedSlashOrBackslash(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}

func forbiddenProxyHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "host", "cookie", "cookie2", "set-cookie":
		return true
	default:
		return false
	}
}

func proxyRequestPlaceholder(r *http.Request, policy ProxyPolicy) string {
	if policy.AuthKind == ProxyAuthHeader {
		return strings.TrimSpace(r.Header.Get(policy.Header))
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, value, ok := strings.Cut(authorization, " ")
	if ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return strings.TrimSpace(value)
	}
	return ""
}

func removeCredentialHeaders(header http.Header, configured string) {
	for _, name := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", CapabilityHeader, configured} {
		if name != "" {
			header.Del(name)
		}
	}
}

func joinProxyPath(basePath, suffix string) string {
	if suffix == "" {
		return basePath
	}
	if basePath == "/" {
		return suffix
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(suffix, "/")
}

func mintProxyRoute() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("mint credential gateway route: %w", err)
	}
	return "/_gitmoot/proxy/" + hex.EncodeToString(random), nil
}

func mintProxyCapability() (string, [sha256.Size]byte, error) {
	random := make([]byte, proxyCapabilityBytes)
	if _, err := rand.Read(random); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("mint credential gateway capability: %w", err)
	}
	return hex.EncodeToString(random), sha256.Sum256(random), nil
}

func proxyRoute(escapedPath string) (string, bool) {
	const prefix = "/_gitmoot/proxy/"
	if !strings.HasPrefix(escapedPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(escapedPath, prefix)
	segment, _, _ := strings.Cut(rest, "/")
	if len(segment) != 32 {
		return prefix + segment, true
	}
	return prefix + segment, true
}

func validatePolicy(policy Policy) (*url.URL, map[string]struct{}, error) {
	raw := strings.TrimSpace(policy.Upstream)
	if raw == "" {
		raw = DefaultAnthropicUpstream
	}
	upstream, err := url.Parse(raw)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Hostname() == "" || upstream.User != nil || upstream.Opaque != "" || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, nil, fmt.Errorf("invalid model gateway upstream %q", raw)
	}
	allowed := make(map[string]struct{}, len(policy.AllowedHosts))
	for _, host := range policy.AllowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			allowed[host] = struct{}{}
		}
	}
	if _, ok := allowed[strings.ToLower(upstream.Hostname())]; !ok {
		return nil, nil, fmt.Errorf("model gateway upstream host %q is not allowlisted", upstream.Hostname())
	}
	return upstream, allowed, nil
}

func mintPlaceholder(jobID string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("mint model gateway placeholder: %w", err)
	}
	cleanID := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, jobID)
	return "gitmoot-kc-" + cleanID + "-" + hex.EncodeToString(random), nil
}

func requestPlaceholder(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Api-Key")); value != "" {
		return value
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, value, ok := strings.Cut(authorization, " ")
	if ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return strings.TrimSpace(value)
	}
	return ""
}

func joinURLPath(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		return requestPath
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(requestPath, "/")
}

func removeHopHeaders(header http.Header) {
	for _, name := range strings.Split(header.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			header.Del(name)
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func streamResponse(w http.ResponseWriter, body io.Reader) {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32*1024)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (g *Gateway) writeLog(method, host string, status int, jobID string) {
	if g.logf != nil {
		g.logf("model gateway request method=%s upstream_host=%s status=%d job_id=%s", method, host, status, jobID)
	}
}

type Registry struct {
	mu       sync.Mutex
	gateways map[string]*Gateway
}

func NewRegistry() *Registry {
	return &Registry{gateways: make(map[string]*Gateway)}
}

func (r *Registry) Gateway(home string, logf LogFunc) (*Gateway, error) {
	if r == nil {
		return nil, errors.New("model gateway registry is unavailable")
	}
	key, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return nil, fmt.Errorf("resolve model gateway home: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.gateways[key]; existing != nil {
		return existing, nil
	}
	gateway, err := Start(logf)
	if err != nil {
		return nil, err
	}
	r.gateways[key] = gateway
	return gateway, nil
}

func (r *Registry) RemoteGateway(home string, logf LogFunc, options RemoteListenerOptions) (*Gateway, error) {
	if r == nil {
		return nil, errors.New("model gateway registry is unavailable")
	}
	key, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return nil, fmt.Errorf("resolve model gateway home: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	gateway := r.gateways[key]
	if gateway == nil {
		gateway, err = Start(logf)
		if err != nil {
			return nil, err
		}
		r.gateways[key] = gateway
	}
	if configured, err := gateway.remoteConfiguredFor(options); configured {
		if err != nil {
			return nil, err
		}
		return gateway, nil
	}
	if err := gateway.EnableRemote(options); err != nil {
		return nil, err
	}
	return gateway, nil
}

func (r *Registry) RevokeSandbox(home, sandboxID string) {
	if r == nil || strings.TrimSpace(sandboxID) == "" {
		return
	}
	key, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return
	}
	r.mu.Lock()
	gateway := r.gateways[key]
	r.mu.Unlock()
	if gateway != nil {
		gateway.RevokeSandbox(sandboxID)
	}
}

func (r *Registry) CloseHome(ctx context.Context, home string) error {
	if r == nil {
		return nil
	}
	key, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return err
	}
	r.mu.Lock()
	gateway := r.gateways[key]
	delete(r.gateways, key)
	r.mu.Unlock()
	if gateway == nil {
		return nil
	}
	return gateway.Close(ctx)
}
