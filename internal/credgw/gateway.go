package credgw

import (
	"context"
	"crypto/rand"
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
	"strings"
	"sync"
	"time"
)

const DefaultAnthropicUpstream = "https://api.anthropic.com"

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
)

// ProxyPolicy pins one generic lease to a complete origin, normalized base
// path, and credential placement. AllowLoopbackHTTP exists solely for tests;
// production callers leave it false and therefore require HTTPS.
type ProxyPolicy struct {
	Upstream          string
	AuthKind          ProxyAuthKind
	Header            string
	AllowLoopbackHTTP bool
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

type Gateway struct {
	listener net.Listener
	server   *http.Server
	client   *http.Client
	logf     LogFunc

	mu           sync.RWMutex
	entries      map[string]entry
	proxyEntries map[string]proxyEntry
	closed       bool
}

type entry struct {
	jobID      string
	credential Credential
	upstream   *url.URL
	allowed    map[string]struct{}
}

type proxyEntry struct {
	jobID       string
	placeholder string
	policy      ProxyPolicy
	upstream    *url.URL
	resolver    CredentialResolver
}

func Start(logf LogFunc) (*Gateway, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for model gateway: %w", err)
	}
	gateway := &Gateway{
		listener: listener,
		client: &http.Client{
			Transport: http.DefaultTransport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("model gateway upstream redirects are disabled")
			},
		},
		logf:         logf,
		entries:      make(map[string]entry),
		proxyEntries: make(map[string]proxyEntry),
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
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, errors.New("credential gateway is not running")
	}
	g.proxyEntries[route] = proxyEntry{
		jobID: jobID, placeholder: placeholder, policy: validated,
		upstream: upstream, resolver: resolver,
	}
	return &Lease{gateway: g, placeholder: placeholder, route: route}, nil
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
	return g.server.Shutdown(ctx)
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
	g.mu.RLock()
	registered, ok := g.proxyEntries[route]
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
	if r.URL.IsAbs() || r.URL.Host != "" || !strings.EqualFold(r.Host, g.listener.Addr().String()) {
		http.Error(w, "request target refused", http.StatusBadRequest)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadRequest, registered.jobID)
		return
	}
	suffix, err := proxyRequestSuffix(r.URL.EscapedPath(), route)
	if err != nil {
		http.Error(w, "request path refused", http.StatusBadRequest)
		g.writeLog(r.Method, registered.upstream.Hostname(), http.StatusBadRequest, registered.jobID)
		return
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
	switch registered.policy.AuthKind {
	case ProxyAuthBearer:
		outbound.Header.Set("Authorization", "Bearer "+resolved.Value)
	case ProxyAuthHeader:
		outbound.Header.Set(registered.policy.Header, resolved.Value)
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
	return err == nil && current.Upstream == want.Upstream && current.AuthKind == want.AuthKind && current.Header == want.Header
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
	for _, name := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", configured} {
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
