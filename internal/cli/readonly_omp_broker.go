package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	readOnlyOmpBrokerBodyLimit  = 16 << 20
	readOnlyOmpBrokerTokenLimit = 16 << 10
)

type readOnlyOmpBrokerConfig struct {
	url   string
	token string
}

type readOnlyOmpBrokerProxy struct {
	server        *http.Server
	listener      net.Listener
	client        *http.Client
	upstream      readOnlyOmpBrokerConfig
	downstreamKey string
	provider      string
}

func readOnlySeatOmpBrokerConfig(environ []string) (readOnlyOmpBrokerConfig, error) {
	const (
		urlName       = "OMP_AUTH_BROKER_URL"
		tokenName     = "OMP_AUTH_BROKER_TOKEN"
		tokenFileName = "OMP_AUTH_BROKER_TOKEN_FILE"
	)
	var config readOnlyOmpBrokerConfig
	var tokenFile, ambientToken string
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case urlName:
			config.url = strings.TrimSpace(value)
		case tokenName:
			ambientToken = strings.TrimSpace(value)
		case tokenFileName:
			tokenFile = strings.TrimSpace(value)
		}
	}
	if ambientToken != "" {
		return readOnlyOmpBrokerConfig{}, fmt.Errorf("read-only omp seat refuses %s in the daemon environment because /proc exposes it; put the token in a mode-0600 file named by %s", tokenName, tokenFileName)
	}
	var missing []string
	if config.url == "" {
		missing = append(missing, urlName)
	}
	if tokenFile == "" {
		missing = append(missing, tokenFileName)
	}
	if len(missing) != 0 {
		return readOnlyOmpBrokerConfig{}, fmt.Errorf("read-only omp seat requires an isolated auth broker; set %s in the daemon environment", strings.Join(missing, " and "))
	}
	parsed, err := url.Parse(config.url)
	validOrigin := err == nil &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" &&
		parsed.User == nil &&
		(parsed.Path == "" || parsed.Path == "/") &&
		parsed.RawPath == "" &&
		parsed.RawQuery == "" &&
		parsed.Fragment == ""
	if validOrigin && parsed.Scheme == "http" {
		hostIP := net.ParseIP(parsed.Hostname())
		validOrigin = parsed.Hostname() == "localhost" || (hostIP != nil && hostIP.IsLoopback())
	}
	if !validOrigin {
		return readOnlyOmpBrokerConfig{}, errors.New("read-only omp seat requires OMP_AUTH_BROKER_URL to be an HTTPS or loopback HTTP origin without credentials, path, query, or fragment")
	}
	config.token, err = readOnlyOmpBrokerToken(tokenFile)
	if err != nil {
		return readOnlyOmpBrokerConfig{}, err
	}
	config.url = strings.TrimRight(config.url, "/")
	return config, nil
}

func readOnlyOmpBrokerToken(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("read-only omp seat requires OMP_AUTH_BROKER_TOKEN_FILE to be an absolute path")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect OMP_AUTH_BROKER_TOKEN_FILE: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 {
		return "", errors.New("read-only omp seat requires OMP_AUTH_BROKER_TOKEN_FILE to be an owner-only regular file (0600 or stricter)")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open OMP_AUTH_BROKER_TOKEN_FILE: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) ||
		!after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 {
		return "", errors.New("OMP_AUTH_BROKER_TOKEN_FILE changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, readOnlyOmpBrokerTokenLimit+1))
	if err != nil || len(data) > readOnlyOmpBrokerTokenLimit {
		return "", errors.New("read OMP_AUTH_BROKER_TOKEN_FILE: token is unavailable or too large")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("OMP_AUTH_BROKER_TOKEN_FILE contains an invalid token")
	}
	return token, nil
}

func readOnlyOmpProvider(model string) (string, error) {
	model = strings.TrimSpace(model)
	provider, modelID, ok := strings.Cut(model, "/")
	if !ok || provider == "" || modelID == "" || strings.ContainsAny(provider, " \t\r\n") {
		return "", fmt.Errorf("read-only omp seat requires a provider-qualified model such as provider/model; got %q", model)
	}
	return provider, nil
}

func startReadOnlyOmpBrokerProxy(config readOnlyOmpBrokerConfig, model string) (*readOnlyOmpBrokerProxy, error) {
	provider, err := readOnlyOmpProvider(model)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("generate read-only omp broker token: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for read-only omp broker: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy := &readOnlyOmpBrokerProxy{
		listener: listener,
		client: &http.Client{
			Transport: transport,
			Timeout:   35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		upstream:      config,
		downstreamKey: base64.RawURLEncoding.EncodeToString(keyBytes),
		provider:      provider,
	}
	proxy.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      40 * time.Second,
		IdleTimeout:       40 * time.Second,
	}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *readOnlyOmpBrokerProxy) writeConfig(stateDir string) error {
	configPath := filepath.Join(stateDir, "config.yml")
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create read-only omp broker config: %w", err)
	}
	body := fmt.Sprintf(
		"auth:\n  broker:\n    url: %s\n    token: %s\n",
		strconv.Quote("http://"+p.listener.Addr().String()),
		strconv.Quote(p.downstreamKey),
	)
	if _, err := io.WriteString(file, body); err != nil {
		file.Close()
		return fmt.Errorf("write read-only omp broker config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close read-only omp broker config: %w", err)
	}
	return nil
}

func (p *readOnlyOmpBrokerProxy) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.server.Shutdown(ctx)
	p.client.CloseIdleConnections()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (p *readOnlyOmpBrokerProxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/healthz" && request.Method == http.MethodGet {
		writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "gitmoot-scoped"})
		return
	}
	if !p.authorized(request.Header.Get("Authorization")) {
		writeOmpBrokerJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot/stream":
		// OMP permanently falls back to conditional snapshot polling after this
		// sentinel. Filtering an unbounded SSE stream would add a second protocol
		// parser to a credential boundary for no functional gain.
		writeOmpBrokerJSON(w, http.StatusNotFound, map[string]any{"error": "stream unsupported by scoped broker"})
	case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
		p.forward(w, request, "credentials", nil)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/usage":
		p.forward(w, request, "reports", nil)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/credentials/disabled":
		query := request.URL.Query()
		query.Set("provider", p.provider)
		request.URL.RawQuery = query.Encode()
		p.forward(w, request, "disabled", nil)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/usage/observed":
		body, err := readOmpBrokerBody(request.Body)
		if err != nil || !brokerBodyUsesOnlyProvider(body, p.provider, true) {
			writeOmpBrokerJSON(w, http.StatusForbidden, map[string]any{"error": "provider outside seat scope"})
			return
		}
		// A reviewer cannot write client identity or usage into the shared vault.
		// A local success preserves OMP's best-effort accounting path.
		writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true})
	case request.Method == http.MethodPost && request.URL.Path == "/v1/usage/stale":
		// Invalidating every broker client's usage cache is outside a seat's
		// authority. OMP treats this notification as best-effort.
		writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		credentialID, action, ok := ompBrokerCredentialRoute(request.Method, request.URL.Path)
		if !ok || !p.credentialAllowed(request.Context(), credentialID) {
			writeOmpBrokerJSON(w, http.StatusForbidden, map[string]any{"error": "broker route outside seat scope"})
			return
		}
		body, err := readOmpBrokerBody(request.Body)
		if err != nil {
			writeOmpBrokerJSON(w, http.StatusBadRequest, map[string]any{"error": "request body too large"})
			return
		}
		if len(body) != 0 && !brokerBodyUsesOnlyProvider(body, p.provider, false) {
			writeOmpBrokerJSON(w, http.StatusForbidden, map[string]any{"error": "provider outside seat scope"})
			return
		}
		if action != "refresh" {
			// Disable/block writes would mutate a shared credential or its
			// availability for every agent. Keep them local to this disposable
			// client by acknowledging without forwarding.
			writeOmpBrokerJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		p.forward(w, request, "entry", body)
	}
}

func (p *readOnlyOmpBrokerProxy) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if len(provided) != len(p.downstreamKey) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(p.downstreamKey)) == 1
}

func ompBrokerCredentialRoute(method, path string) (int64, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "credential" {
		return 0, "", false
	}
	allowed := (method == http.MethodPost && (parts[3] == "refresh" || parts[3] == "disable" || parts[3] == "block")) ||
		(method == http.MethodDelete && parts[3] == "blocks")
	if !allowed {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	return id, parts[3], err == nil && id > 0
}

func (p *readOnlyOmpBrokerProxy) credentialAllowed(ctx context.Context, credentialID int64) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream.url+"/v1/snapshot", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+p.upstream.token)
	response, err := p.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, readOnlyOmpBrokerBodyLimit+1))
	if err != nil || len(body) > readOnlyOmpBrokerBodyLimit {
		return false
	}
	var snapshot struct {
		Credentials []struct {
			ID       int64  `json:"id"`
			Provider string `json:"provider"`
		} `json:"credentials"`
	}
	if json.Unmarshal(body, &snapshot) != nil {
		return false
	}
	for _, credential := range snapshot.Credentials {
		if credential.ID == credentialID {
			return credential.Provider == p.provider
		}
	}
	return false
}

func (p *readOnlyOmpBrokerProxy) forward(w http.ResponseWriter, incoming *http.Request, responseField string, body []byte) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else if incoming.Body != nil {
		reader = incoming.Body
	}
	upstreamRequest, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, p.upstream.url+incoming.URL.RequestURI(), reader)
	if err != nil {
		writeOmpBrokerJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed"})
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+p.upstream.token)
	for _, name := range []string{"Accept", "Content-Type", "If-None-Match", "OMP-Auth-Broker-Capabilities"} {
		if value := incoming.Header.Get(name); value != "" {
			upstreamRequest.Header.Set(name, value)
		}
	}
	response, err := p.client.Do(upstreamRequest)
	if err != nil {
		writeOmpBrokerJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream broker unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified || response.StatusCode == http.StatusNoContent {
		w.WriteHeader(response.StatusCode)
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, readOnlyOmpBrokerBodyLimit+1))
	if err != nil || len(responseBody) > readOnlyOmpBrokerBodyLimit {
		writeOmpBrokerJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream broker response too large"})
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeOmpBrokerJSON(w, response.StatusCode, map[string]any{"error": "upstream broker request failed"})
		return
	}
	responseBody, err = filterOmpBrokerPayload(responseBody, p.provider, responseField)
	if err != nil {
		writeOmpBrokerJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream broker response invalid"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

func filterOmpBrokerPayload(body []byte, provider, responseField string) ([]byte, error) {
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	if responseField == "entry" {
		entry, ok := object[responseField].(map[string]any)
		if !ok || entry["provider"] != provider {
			return nil, errors.New("broker returned an invalid scoped credential")
		}
		return json.Marshal(object)
	}
	items, ok := object[responseField].([]any)
	if !ok {
		return nil, fmt.Errorf("broker response omitted %q array", responseField)
	}
	filtered := items[:0]
	for _, item := range items {
		record, ok := item.(map[string]any)
		if ok && record["provider"] == provider {
			filtered = append(filtered, item)
		}
	}
	object[responseField] = filtered
	return json.Marshal(object)
}

func brokerBodyUsesOnlyProvider(body []byte, provider string, requireProvider bool) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return !requireProvider
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	seen := false
	var visit func(any) bool
	visit = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "provider" || key == "providerKey" {
					text, ok := child.(string)
					if !ok || text != provider {
						return false
					}
					seen = true
				}
				if !visit(child) {
					return false
				}
			}
		case []any:
			for _, child := range typed {
				if !visit(child) {
					return false
				}
			}
		}
		return true
	}
	return visit(value) && (!requireProvider || seen)
}

func readOmpBrokerBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, readOnlyOmpBrokerBodyLimit+1))
	if err != nil || len(data) > readOnlyOmpBrokerBodyLimit {
		return nil, errors.New("broker request body too large")
	}
	return data, nil
}

func writeOmpBrokerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
