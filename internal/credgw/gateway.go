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

	mu      sync.RWMutex
	entries map[string]entry
	closed  bool
}

type entry struct {
	jobID      string
	credential Credential
	upstream   *url.URL
	allowed    map[string]struct{}
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
		logf:    logf,
		entries: make(map[string]entry),
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
	g.mu.Unlock()
	return g.server.Shutdown(ctx)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
