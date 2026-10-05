package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// sandboxdCapacityPath is sandboxd's capacity report (sandboxd #30). It is a
// sandboxd extension; cloud E2B does not serve it.
const sandboxdCapacityPath = "/sandboxd/capacity"

// Capacity errors. Each Capacity failure wraps exactly one of them.
var (
	// ErrCapacityUnsupported means the gateway answered 404: a sandboxd older
	// than the capacity endpoint. Only this answer may fall back to a fixed
	// ceiling, because it proves the gateway cannot report capacity at all.
	ErrCapacityUnsupported = errors.New("sandboxd capacity endpoint is not supported (HTTP 404): sandboxd predates GET /sandboxd/capacity")
	// ErrCapacityAuth means the gateway refused the API key (401/403). It
	// never frees by waiting.
	ErrCapacityAuth = errors.New("sandboxd refused the API key for GET /sandboxd/capacity")
	// ErrCapacityUnavailable is every other failure: transport error, timeout,
	// 5xx, 429, or a response that is not the capacity report. It may clear.
	ErrCapacityUnavailable = errors.New("sandboxd capacity is unavailable")
)

// Capacity is sandboxd's capacity report. Totals count only online workers.
type Capacity struct {
	TotalSlots int                `json:"totalSlots"`
	UsedSlots  int                `json:"usedSlots"`
	FreeSlots  int                `json:"freeSlots"`
	Templates  []TemplateCapacity `json:"templates"`
	Workers    []WorkerCapacity   `json:"workers"`
}

// TemplateCapacity is one template's slots across the online workers that
// serve it. A worker serving two templates counts in both.
type TemplateCapacity struct {
	TemplateID string `json:"templateID"`
	Arch       string `json:"arch"`
	TotalSlots int    `json:"totalSlots"`
	FreeSlots  int    `json:"freeSlots"`
}

// WorkerCapacity is the part of one worker's report Gitmoot uses.
type WorkerCapacity struct {
	WorkerID string `json:"workerID"`
	Online   bool   `json:"online"`
	MaxVMs   int    `json:"maxVMs"`
	Error    string `json:"error"`
}

// Template returns templateID's entry; ok is false when no online worker
// serves it.
func (c Capacity) Template(templateID string) (TemplateCapacity, bool) {
	for _, template := range c.Templates {
		if template.TemplateID == templateID {
			return template, true
		}
	}
	return TemplateCapacity{}, false
}

// OfflineWorkers lists the IDs of workers the gateway reports offline.
func (c Capacity) OfflineWorkers() []string {
	var offline []string
	for _, worker := range c.Workers {
		if !worker.Online {
			offline = append(offline, worker.WorkerID)
		}
	}
	return offline
}

// Capacity reads GET /sandboxd/capacity. It does not use doJSONState, whose
// 404 handling treats a non-create 404 as inconclusive: here a 404 is the
// definite answer "this sandboxd has no capacity endpoint".
func (c *Client) Capacity(ctx context.Context) (Capacity, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	endpoint := strings.TrimRight(c.baseURL.String(), "/") + sandboxdCapacityPath
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "build request: %v", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "request failed: %v", err)
	}
	defer resp.Body.Close()
	body, oversized, err := readProviderResponseBody(resp.Body)
	if err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "HTTP %d and the response could not be read: %v", resp.StatusCode, err)
	}
	if oversized {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "HTTP %d with an oversized response", resp.StatusCode)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Capacity{}, c.capacityError(ErrCapacityUnsupported, "HTTP 404: %s", body)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Capacity{}, c.capacityError(ErrCapacityAuth, "HTTP %d: %s", resp.StatusCode, body)
	case resp.StatusCode != http.StatusOK:
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "HTTP %d: %s", resp.StatusCode, body)
	}
	if err := requireJSONMediaType(resp.Header); err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "response is not JSON: %v", err)
	}
	var report Capacity
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&report); err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "decode response: %v", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "decode response: %v", err)
	}
	if report.TotalSlots < 0 || report.FreeSlots < 0 || report.UsedSlots < 0 {
		return Capacity{}, c.capacityError(ErrCapacityUnavailable, "negative slot counts in response")
	}
	for _, template := range report.Templates {
		if strings.TrimSpace(template.TemplateID) == "" || template.TotalSlots < 0 || template.FreeSlots < 0 {
			return Capacity{}, c.capacityError(ErrCapacityUnavailable, "invalid template entry in response")
		}
	}
	return report, nil
}

// capacityError wraps kind with a redacted description of the failure.
func (c *Client) capacityError(kind error, format string, args ...any) error {
	message := workflow.RedactedStderrTail(fmt.Sprintf("GET %s: ", sandboxdCapacityPath)+fmt.Sprintf(format, args...), c.apiKey)
	return fmt.Errorf("%w: %s", kind, message)
}
