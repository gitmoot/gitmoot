// Package e2b implements E2B control-plane and envd data-plane clients.
//
// It deliberately does not implement execbackend.Backend or wire either client
// into the daemon; provider lifecycle integration remains a separate slice.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	DefaultBaseURL               = "https://api.e2b.app"
	DefaultSandboxDomain         = "e2b.app"
	DefaultRequestTimeout        = 15 * time.Second
	maxProviderResponseBodyBytes = 1 << 20
	minAPIKeyLength              = 8
	listSandboxesPageSize        = 100
	maxListPages                 = 100
	maxNextTokenBytes            = 4096
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
	Domain      string            `json:"domain,omitempty"`
}

// EnvdCredential is a sandbox-scoped envd access credential. Its token is
// deliberately inaccessible and every standard rendering is redacted.
type EnvdCredential struct {
	token string
}

func (EnvdCredential) String() string   { return "[REDACTED]" }
func (EnvdCredential) GoString() string { return "[REDACTED]" }

type listedSandbox struct {
	ID          *string           `json:"sandboxID"`
	TemplateID  *string           `json:"templateID"`
	Alias       string            `json:"alias,omitempty"`
	ClientID    string            `json:"clientID,omitempty"`
	EnvdVersion *string           `json:"envdVersion"`
	StartedAt   *time.Time        `json:"startedAt"`
	EndAt       *time.Time        `json:"endAt"`
	CPUCount    *int32            `json:"cpuCount"`
	MemoryMB    *int32            `json:"memoryMB"`
	DiskSizeMB  *int32            `json:"diskSizeMB"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	State       *string           `json:"state"`
}

type sandboxPage struct {
	items []json.RawMessage
}

func (p *sandboxPage) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('[') {
		return errors.New("E2B sandbox inventory must be a JSON array")
	}

	var page []json.RawMessage
	for decoder.More() {
		var sandbox json.RawMessage
		if err := decoder.Decode(&sandbox); err != nil {
			return err
		}
		page = append(page, sandbox)
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim(']') {
		return errors.New("E2B sandbox inventory has no closing array delimiter")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	p.items = page
	return nil
}

type inventoryValidation struct {
	runningLowerBound uint64
	runningCollected  uint64
	runningObserved   bool
}

func (v *inventoryValidation) acceptPage(headers http.Header, page sandboxPage, collected int, terminal bool) ([]Sandbox, error) {
	totalRunning, present, err := responseTotalRunning(headers)
	if err != nil {
		return nil, err
	}
	if present && (!v.runningObserved || totalRunning > v.runningLowerBound) {
		v.runningLowerBound = totalRunning
		v.runningObserved = true
	}

	sandboxes := make([]Sandbox, 0, len(page.items))
	for i := range page.items {
		sandbox, err := decodeListedSandbox(page.items[i])
		if err != nil {
			return nil, fmt.Errorf("response item %d: %w", collected+i, err)
		}
		if sandbox.State == "running" {
			v.runningCollected++
		}
		sandboxes = append(sandboxes, sandbox)
	}

	if terminal && v.runningObserved && v.runningCollected < v.runningLowerBound {
		return nil, fmt.Errorf("collected %d running sandboxes, fewer than X-Total-Running lower bound %d", v.runningCollected, v.runningLowerBound)
	}
	return sandboxes, nil
}

func (s listedSandbox) sandbox() (Sandbox, error) {
	if strings.TrimSpace(*s.ID) == "" {
		return Sandbox{}, errors.New("missing sandboxID")
	}
	return Sandbox{
		ID:          *s.ID,
		TemplateID:  *s.TemplateID,
		Alias:       s.Alias,
		ClientID:    s.ClientID,
		EnvdVersion: *s.EnvdVersion,
		StartedAt:   *s.StartedAt,
		EndAt:       *s.EndAt,
		CPUCount:    *s.CPUCount,
		MemoryMB:    *s.MemoryMB,
		DiskSizeMB:  *s.DiskSizeMB,
		Metadata:    s.Metadata,
		State:       *s.State,
	}, nil
}

func responseTotalRunning(headers http.Header) (uint64, bool, error) {
	values := headers.Values("X-Total-Running")
	if len(values) == 0 {
		return 0, false, nil
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return 0, false, errors.New("invalid empty X-Total-Running header")
	}
	total, err := strconv.ParseUint(strings.TrimSpace(values[0]), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid X-Total-Running value %q", values[0])
	}
	return total, true, nil
}

func responseNextToken(headers http.Header) (string, bool, error) {
	values := headers.Values("X-Next-Token")
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", false, fmt.Errorf("expected one X-Next-Token value, got %d", len(values))
	}
	token := values[0]
	if token == "" || strings.TrimSpace(token) != token {
		return "", false, errors.New("invalid empty or whitespace-padded X-Next-Token header")
	}
	if len(token) > maxNextTokenBytes {
		return "", false, fmt.Errorf("X-Next-Token exceeds %d bytes", maxNextTokenBytes)
	}
	return token, true, nil
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

// MetricsObservation preserves successful metrics separately from an
// inconclusive read.
type MetricsObservation struct {
	State   State
	Metrics []Metric
}

type createPayload struct {
	TemplateID string `json:"templateID"`
	Timeout    int32  `json:"timeout"`
	// Get cannot prove all-state absence, so TTL expiry must kill rather than pause.
	AutoPause bool `json:"autoPause"`
	// Secure makes envd reject unauthenticated traffic. Create verifies the
	// response token so a provider that ignores this request cannot look healthy.
	Secure   bool              `json:"secure"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Env      map[string]string `json:"envVars,omitempty"`
}

type createResponse struct {
	Sandbox
	EnvdAccessToken string  `json:"envdAccessToken"`
	Domain          *string `json:"domain"`
}

// Create starts a sandbox with a required provider-side TTL.
func (c *Client) Create(ctx context.Context, templateID string, ttl time.Duration, options CreateOptions) (Sandbox, EnvdCredential, error) {
	if strings.TrimSpace(templateID) == "" {
		return Sandbox{}, EnvdCredential{}, errors.New("E2B template ID is required")
	}
	ttlSeconds, err := durationSeconds(ttl)
	if err != nil {
		return Sandbox{}, EnvdCredential{}, fmt.Errorf("E2B create TTL: %w", err)
	}
	var response createResponse
	_, err = c.doJSON(ctx, http.MethodPost, "/sandboxes", createPayload{
		TemplateID: templateID,
		Timeout:    ttlSeconds,
		AutoPause:  false,
		Secure:     true,
		Metadata:   options.Metadata,
		Env:        options.Env,
	}, http.StatusCreated, &response)
	if err != nil {
		return Sandbox{}, EnvdCredential{}, err
	}
	if strings.TrimSpace(response.ID) == "" {
		return Sandbox{}, EnvdCredential{}, c.errorf(nil, "create sandbox: malformed response: missing sandboxID")
	}
	if strings.TrimSpace(response.EnvdAccessToken) == "" {
		return Sandbox{}, EnvdCredential{}, c.errorf(nil, "create sandbox: secure response is missing envdAccessToken")
	}
	domain := DefaultSandboxDomain
	if response.Domain != nil && strings.TrimSpace(*response.Domain) != "" {
		domain = strings.TrimSpace(*response.Domain)
	}
	response.Sandbox.Domain = domain
	return response.Sandbox, EnvdCredential{token: response.EnvdAccessToken}, nil
}

// List returns all sandboxes in the provider response.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var sandboxes []Sandbox
	var validation inventoryValidation
	nextToken := ""
	for pageNumber := 1; pageNumber <= maxListPages; pageNumber++ {
		query := url.Values{"limit": []string{fmt.Sprint(listSandboxesPageSize)}}
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		var page sandboxPage
		headers, err := c.doJSON(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, http.StatusOK, &page)
		if err != nil {
			return nil, err
		}
		next, hasNext, err := responseNextToken(headers)
		if err != nil {
			return nil, c.errorf(err, "list sandboxes: invalid continuation")
		}
		terminal := !hasNext
		// Keep this request unfiltered so paused sandboxes remain visible to the
		// reaper. For unfiltered responses X-Total-Running is optional and counts
		// only running sandboxes, so it is a lower-bound consistency check and
		// cannot establish all-state absence. List data never authorizes Get to
		// return Gone.
		accepted, err := validation.acceptPage(headers, page, len(sandboxes), terminal)
		if err != nil {
			return nil, c.errorf(err, "list sandboxes: invalid inventory")
		}
		sandboxes = append(sandboxes, accepted...)
		if terminal {
			return sandboxes, nil
		}
		nextToken = next
	}
	return nil, c.errorf(nil, "list sandboxes: exceeded %d pages", maxListPages)
}

// Get observes one sandbox. E2B exposes no all-state inventory total, so an
// inconclusive ID response remains Unknown; List cannot prove absence safely.
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
	if strings.TrimSpace(sandbox.ID) == "" {
		return Observation{State: Unknown}, c.errorf(nil, "get sandbox: malformed response: missing sandboxID")
	}
	return Observation{State: Present, Sandbox: &sandbox}, nil
}

// Delete requests sandbox termination. A successful deletion is Gone; failed
// or inconclusive communication remains Unknown.
func (c *Client) Delete(ctx context.Context, sandboxID string) (State, error) {
	path, err := sandboxPath(sandboxID, "")
	if err != nil {
		return Unknown, err
	}
	state, _, err := c.doJSONState(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil)
	if err != nil {
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
		// A PER-ID 404 IS AMBIGUOUS AND A COLLECTION 404 IS NOT. For GET or DELETE
		// on /sandboxes/<id>, "never existed" and "already gone" are
		// indistinguishable, and this epic treats that as unresolvable without a
		// successful list in the same pass - so it stays inconclusive.
		//
		// POST /sandboxes is different: the 404 is about the COLLECTION, not about
		// any instance. An unknown template, or a misrouted e2b_domain reaching a
		// different service, means nothing was allocated. Round 2 review of #2226
		// made this argument and it is better than the blanket rule I had: leaving
		// a create-404 inconclusive stranded its reservation until TTL, the exact
		// leak this work exists to close.
		if operationForRequest(method, path) == OperationCreate {
			return Unknown, resp.Header, &RequestRefusedError{
				StatusCode: resp.StatusCode, Operation: OperationCreate,
				Err: c.errorf(nil, "%s %s: E2B returned HTTP %d: %s", method, path, resp.StatusCode, responseBody),
			}
		}
		return Unknown, resp.Header, c.errorf(nil, "%s %s: E2B returned inconclusive HTTP %d: %s", method, path, resp.StatusCode, responseBody)
	}
	if resp.StatusCode != expectedStatus {
		refusal := c.errorf(nil, "%s %s: E2B returned HTTP %d: %s", method, path, resp.StatusCode, responseBody)
		if requestRefused(resp.StatusCode) {
			refusal = &RequestRefusedError{StatusCode: resp.StatusCode, Operation: operationForRequest(method, path), Err: refusal}
		}
		return Unknown, resp.Header, refusal
	}
	if output == nil {
		return Present, resp.Header, nil
	}
	if err := requireJSONMediaType(resp.Header); err != nil {
		return Unknown, resp.Header, c.errorf(err, "%s %s: E2B response is not authoritative JSON", method, path)
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

func requireJSONMediaType(headers http.Header) error {
	raw := strings.TrimSpace(headers.Get("Content-Type"))
	if raw == "" {
		return errors.New("missing Content-Type")
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("invalid Content-Type %q", raw)
	}
	if !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("Content-Type %q is not application/json", mediaType)
	}
	return nil
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

// RequestRefusedError marks a provider response that is AUTHORITATIVE PROOF
// NOTHING WAS ALLOCATED: the request was rejected on its own terms, before any
// sandbox could exist.
//
// The distinction is load-bearing for the cost ledger. A transport failure is
// genuinely ambiguous - the provider may have allocated a sandbox whose response
// never arrived - so its reservation must stay held until inventory resolves it.
// A 4xx validation refusal is not ambiguous, and treating it as though it were
// STRANDS THE RESERVATION: measured on this box, an HTTP 400 ("Timeout cannot be
// greater than 1 hours") left $1.00 reserved in state "provisioning" holding the
// only concurrency slot, and with cost_max_concurrent = 1 every subsequent
// remote job was refused for the four hours until the row's TTL expired.
//
// This is the recurring defect in this epic once more: one value standing for
// two different facts. "Provision returned an error" meant both "maybe
// allocated" and "definitely did not allocate".
//
// 5xx and 429 are deliberately NOT included. A server error may follow a
// completed allocation, and a rate-limit response can race one; both stay
// ambiguous and keep the fail-safe hold.
type RequestRefusedError struct {
	StatusCode int
	// Operation names the provider call that was refused. The ledger releases a
	// reservation only for OperationCreate: a refusal from a cleanup Delete or a
	// status Get says nothing about whether the CREATE allocated anything.
	Operation string
	Err       error
}

// Provider operations that can carry a refusal. Only OperationCreate is proof of
// non-allocation for the cost ledger.
const (
	OperationCreate     = "create"
	OperationGet        = "get"
	OperationDelete     = "delete"
	OperationSetTimeout = "set_timeout"
	OperationMetrics    = "metrics"
	OperationOther      = "other"
)

func (e *RequestRefusedError) Error() string { return e.Err.Error() }
func (e *RequestRefusedError) Unwrap() error { return e.Err }

// requestRefused reports whether a status code proves the provider rejected the
// request without allocating anything.
// operationForRequest classifies a provider call so the ledger can tell a
// refused CREATE from a refused cleanup.
func operationForRequest(method, path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	switch {
	case method == http.MethodPost && trimmed == "/sandboxes":
		return OperationCreate
	case method == http.MethodDelete:
		return OperationDelete
	case method == http.MethodGet && strings.Contains(trimmed, "/metrics"):
		return OperationMetrics
	case method == http.MethodGet:
		return OperationGet
	case method == http.MethodPost && strings.Contains(trimmed, "/timeout"):
		return OperationSetTimeout
	default:
		return OperationOther
	}
}

func requestRefused(status int) bool {
	switch status {
	// 404 AND 410 ARE DELIBERATELY ABSENT HERE, and handled earlier instead.
	// doJSONState reaches its own 404/410 branch before this check, where a
	// CREATE 404 becomes a refusal (the collection cannot be missing and an
	// instance still be allocated) while a per-id 404 stays inconclusive, because
	// "never existed" and "already gone" are indistinguishable on this provider.
	// Listing them here as well would be dead code.
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusForbidden, http.StatusMethodNotAllowed,
		http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	default:
		return false
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
