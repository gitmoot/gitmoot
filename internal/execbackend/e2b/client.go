// Package e2b implements the E2B control-plane REST client.
//
// It deliberately does not implement execbackend.Backend: command execution
// and file transfer use the sandbox envd protocol and remain outside this
// package.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	DefaultBaseURL               = "https://api.e2b.app"
	DefaultRequestTimeout        = 15 * time.Second
	maxProviderResponseBodyBytes = 1 << 20
	minAPIKeyLength              = 8
	listSandboxesPageSize        = 100
)

// State distinguishes a confirmed live sandbox, provider-confirmed absence,
// and an inconclusive observation. Unknown must be treated as potentially live.
type State uint8

const (
	Unknown State = iota
	Present
	Gone
)

func (s State) String() string {
	switch s {
	case Present:
		return "present"
	case Gone:
		return "gone"
	default:
		return "unknown"
	}
}

// Options configures a Client. Zero RequestTimeout selects the bounded default.
type Options struct {
	BaseURL        string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// Client calls E2B's control plane. It never logs and never reads credentials
// from ambient process state.
type Client struct {
	baseURL        *url.URL
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
}

// NewClient constructs a bounded E2B control-plane client.
func NewClient(apiKey string, options Options) (*Client, error) {
	if len(strings.TrimSpace(apiKey)) < minAPIKeyLength {
		return nil, fmt.Errorf("E2B API key must be at least %d characters", minAPIKeyLength)
	}
	base := strings.TrimSpace(options.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse E2B base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("E2B base URL must be an absolute HTTP(S) URL without query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	timeout := options.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	if timeout < 0 {
		return nil, errors.New("E2B request timeout must be positive")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	httpClientCopy := *httpClient
	// Never forward X-API-Key through a provider redirect.
	httpClientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: parsed, apiKey: apiKey, httpClient: &httpClientCopy, requestTimeout: timeout}, nil
}

// CreateOptions contains optional creation metadata. The provider TTL is a
// positional Create argument so a caller cannot omit it by construction.
type CreateOptions struct {
	Metadata map[string]string
	Env      map[string]string
}

// Sandbox contains the control-plane fields needed to identify and track a
// sandbox. Envd access credentials are intentionally not represented here.
type Sandbox struct {
	ID          string            `json:"sandboxID"`
	TemplateID  string            `json:"templateID"`
	Alias       string            `json:"alias,omitempty"`
	ClientID    string            `json:"clientID,omitempty"`
	EnvdVersion string            `json:"envdVersion,omitempty"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	EndAt       time.Time         `json:"endAt,omitempty"`
	CPUCount    int32             `json:"cpuCount,omitempty"`
	MemoryMB    int32             `json:"memoryMB,omitempty"`
	DiskSizeMB  int32             `json:"diskSizeMB,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	State       string            `json:"state,omitempty"`
}

// Observation is the three-state result of looking up one sandbox.
type Observation struct {
	State   State
	Sandbox *Sandbox
}

// Metric is one E2B sandbox metrics sample.
type Metric struct {
	TimestampUnix int64   `json:"timestampUnix"`
	CPUCount      int32   `json:"cpuCount"`
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	MemoryUsed    int64   `json:"memUsed"`
	MemoryTotal   int64   `json:"memTotal"`
	MemoryCache   int64   `json:"memCache"`
	DiskUsed      int64   `json:"diskUsed"`
	DiskTotal     int64   `json:"diskTotal"`
}

// MetricsObservation preserves provider-confirmed absence separately from an
// inconclusive metrics read.
type MetricsObservation struct {
	State   State
	Metrics []Metric
}

type createPayload struct {
	TemplateID string            `json:"templateID"`
	Timeout    int32             `json:"timeout"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Env        map[string]string `json:"envVars,omitempty"`
}

// Create starts a sandbox with a required provider-side TTL.
func (c *Client) Create(ctx context.Context, templateID string, ttl time.Duration, options CreateOptions) (Sandbox, error) {
	if strings.TrimSpace(templateID) == "" {
		return Sandbox{}, errors.New("E2B template ID is required")
	}
	ttlSeconds, err := durationSeconds(ttl)
	if err != nil {
		return Sandbox{}, fmt.Errorf("E2B create TTL: %w", err)
	}
	var sandbox Sandbox
	_, err = c.doJSON(ctx, http.MethodPost, "/sandboxes", createPayload{
		TemplateID: templateID,
		Timeout:    ttlSeconds,
		Metadata:   options.Metadata,
		Env:        options.Env,
	}, http.StatusCreated, &sandbox)
	if err != nil {
		return Sandbox{}, err
	}
	if strings.TrimSpace(sandbox.ID) == "" {
		return Sandbox{}, c.errorf(nil, "create sandbox: malformed response: missing sandboxID")
	}
	return sandbox, nil
}

// List returns all sandboxes in the provider response.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var sandboxes []Sandbox
	nextToken := ""
	seenTokens := make(map[string]struct{})
	for {
		query := url.Values{"limit": []string{fmt.Sprint(listSandboxesPageSize)}}
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		var page []Sandbox
		headers, err := c.doJSON(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, http.StatusOK, &page)
		if err != nil {
			return nil, err
		}
		for i := range page {
			if strings.TrimSpace(page[i].ID) == "" {
				return nil, c.errorf(nil, "list sandboxes: malformed response item %d: missing sandboxID", len(sandboxes)+i)
			}
		}
		sandboxes = append(sandboxes, page...)

		nextToken = strings.TrimSpace(headers.Get("X-Next-Token"))
		if nextToken == "" {
			return sandboxes, nil
		}
		if _, duplicate := seenTokens[nextToken]; duplicate {
			return nil, c.errorf(nil, "list sandboxes: repeated pagination token %q", nextToken)
		}
		seenTokens[nextToken] = struct{}{}
	}
}

// Get observes one sandbox. Only 404 or 410 means Gone; every failed or
// undecodable read remains Unknown with an error.
func (c *Client) Get(ctx context.Context, sandboxID string) (Observation, error) {
	path, err := sandboxPath(sandboxID, "")
	if err != nil {
		return Observation{State: Unknown}, err
	}
	var sandbox Sandbox
	status, _, err := c.doJSONState(ctx, http.MethodGet, path, nil, http.StatusOK, &sandbox)
	if err != nil {
		return Observation{State: status}, err
	}
	if status == Gone {
		return Observation{State: Gone}, nil
	}
	if strings.TrimSpace(sandbox.ID) == "" {
		return Observation{State: Unknown}, c.errorf(nil, "get sandbox: malformed response: missing sandboxID")
	}
	return Observation{State: Present, Sandbox: &sandbox}, nil
}

// Delete requests sandbox termination. A successful deletion or an already
// absent sandbox is Gone; failed communication remains Unknown.
func (c *Client) Delete(ctx context.Context, sandboxID string) (State, error) {
	path, err := sandboxPath(sandboxID, "")
	if err != nil {
		return Unknown, err
	}
	state, _, err := c.doJSONState(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil)
	if err != nil || state == Gone {
		return state, err
	}
	return Gone, nil
}

// SetTimeout replaces the provider-side TTL from the time of the request.
func (c *Client) SetTimeout(ctx context.Context, sandboxID string, ttl time.Duration) (State, error) {
	seconds, err := durationSeconds(ttl)
	if err != nil {
		return Unknown, fmt.Errorf("E2B sandbox timeout: %w", err)
	}
	path, err := sandboxPath(sandboxID, "/timeout")
	if err != nil {
		return Unknown, err
	}
	state, _, err := c.doJSONState(ctx, http.MethodPost, path, struct {
		Timeout int32 `json:"timeout"`
	}{Timeout: seconds}, http.StatusNoContent, nil)
	return state, err
}

// Metrics reads resource samples without collapsing absence and failed reads.
func (c *Client) Metrics(ctx context.Context, sandboxID string) (MetricsObservation, error) {
	path, err := sandboxPath(sandboxID, "/metrics")
	if err != nil {
		return MetricsObservation{State: Unknown}, err
	}
	var metrics []Metric
	status, _, err := c.doJSONState(ctx, http.MethodGet, path, nil, http.StatusOK, &metrics)
	if err != nil {
		return MetricsObservation{State: status}, err
	}
	if status == Gone {
		return MetricsObservation{State: Gone}, nil
	}
	return MetricsObservation{State: Present, Metrics: metrics}, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, input any, expectedStatus int, output any) (http.Header, error) {
	state, headers, err := c.doJSONState(ctx, method, path, input, expectedStatus, output)
	if err != nil {
		return nil, err
	}
	if state != Present {
		return nil, c.errorf(nil, "%s %s: E2B endpoint was not found", method, path)
	}
	return headers, nil
}

func (c *Client) doJSONState(ctx context.Context, method, path string, input any, expectedStatus int, output any) (State, http.Header, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return Unknown, nil, c.errorf(err, "%s %s: encode request", method, path)
		}
		body = bytes.NewReader(encoded)
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	endpoint := strings.TrimRight(c.baseURL.String(), "/") + path
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return Unknown, nil, c.errorf(err, "%s %s: build request", method, path)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Unknown, nil, c.errorf(err, "%s %s: control-plane request failed", method, path)
	}
	defer resp.Body.Close()
	responseBody, oversized, readErr := readProviderResponseBody(resp.Body)
	if readErr != nil {
		return Unknown, resp.Header, c.errorf(readErr, "%s %s: E2B returned HTTP %d and its response could not be read", method, path, resp.StatusCode)
	}
	if oversized {
		return Unknown, resp.Header, c.errorf(nil, "%s %s: E2B returned HTTP %d with an oversized response", method, path, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if isProviderAbsence(responseBody, resp.StatusCode) {
			return Gone, resp.Header, nil
		}
		return Unknown, resp.Header, c.errorf(nil, "%s %s: E2B returned inconclusive HTTP %d: %s", method, path, resp.StatusCode, responseBody)
	}
	if resp.StatusCode != expectedStatus {
		return Unknown, resp.Header, c.errorf(nil, "%s %s: E2B returned HTTP %d: %s", method, path, resp.StatusCode, responseBody)
	}
	if output == nil {
		return Present, resp.Header, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	if err := decoder.Decode(output); err != nil {
		return Unknown, resp.Header, c.errorf(err, "%s %s: decode E2B response", method, path)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Unknown, resp.Header, c.errorf(err, "%s %s: decode E2B response", method, path)
	}
	return Present, resp.Header, nil
}

func (c *Client) errorf(cause error, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if cause != nil {
		message += ": " + cause.Error()
	}
	return &clientError{
		message: workflow.RedactedStderrTail(message, c.apiKey),
		match:   contextErrorIdentity(cause),
	}
}

type clientError struct {
	message string
	match   error
}

func (e *clientError) Error() string { return e.message }

func (e *clientError) Is(target error) bool {
	return e.match != nil && errors.Is(e.match, target)
}

func contextErrorIdentity(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func readProviderResponseBody(body io.Reader) (data []byte, oversized bool, err error) {
	data, err = io.ReadAll(io.LimitReader(body, maxProviderResponseBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxProviderResponseBodyBytes {
		return nil, true, nil
	}
	return data, false, nil
}

func isProviderAbsence(data []byte, status int) bool {
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return false
	}
	return requireJSONEOF(decoder) == nil && response.Code == status && strings.TrimSpace(response.Message) != ""
}

func durationSeconds(ttl time.Duration) (int32, error) {
	if ttl <= 0 {
		return 0, errors.New("must be positive")
	}
	if ttl%time.Second != 0 {
		return 0, errors.New("must be a whole number of seconds")
	}
	seconds := ttl / time.Second
	if seconds > math.MaxInt32 {
		return 0, errors.New("exceeds provider integer range")
	}
	return int32(seconds), nil
}

func sandboxPath(sandboxID, suffix string) (string, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return "", errors.New("E2B sandbox ID is required")
	}
	return "/sandboxes/" + url.PathEscape(sandboxID) + suffix, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}
