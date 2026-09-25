package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxResponseBytes bounds how much of a response is read. An answer is small;
// anything far larger is not a response this client can trust.
const maxResponseBytes = 4 << 20

// APIError is a failed evaluation, with whether retrying could help.
type APIError struct {
	Status    int
	Message   string
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return "jev: " + e.Message
	}
	return fmt.Sprintf("jev: HTTP %d: %s", e.Status, e.Message)
}

// Client calls the evaluation endpoint with bounded time, retries, and
// request rate.
type Client struct {
	Endpoint   string
	APIKey     string
	HTTP       *http.Client
	Timeout    time.Duration
	MaxRetries int
	// MinInterval spaces requests from one client.
	MinInterval time.Duration
	// BackoffBase and MaxBackoff shape exponential backoff. Retry-After is
	// honoured but never beyond MaxBackoff.
	BackoffBase time.Duration
	MaxBackoff  time.Duration
	// Sleep and Now are injectable so tests exercise backoff without waiting.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time

	mu          sync.Mutex
	lastRequest time.Time
}

// Exchange is one completed call: what was sent, what came back, and how many
// attempts it took.
type Exchange struct {
	Body     []byte
	Response Response
	Attempts int
}

// Evaluate sends one request and returns the parsed response.
func (c *Client) Evaluate(ctx context.Context, request Request) (Exchange, error) {
	body, err := Encode(request)
	if err != nil {
		return Exchange{}, &APIError{Message: "encode request: " + err.Error()}
	}
	exchange := Exchange{Body: body}

	// At least one attempt: a negative MaxRetries must not return success
	// without ever contacting the server.
	attempts := max(c.MaxRetries, 0) + 1
	var last error
	for attempt := range attempts {
		if err := c.pace(ctx); err != nil {
			return exchange, &APIError{Message: "cancelled: " + err.Error()}
		}
		exchange.Attempts++
		response, wait, err := c.once(ctx, body)
		if err == nil {
			exchange.Response = response
			return exchange, nil
		}
		last = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable || attempt == attempts-1 {
			break
		}
		if wait <= 0 {
			wait = c.backoff(attempt)
		}
		if wait > c.maxBackoff() {
			wait = c.maxBackoff()
		}
		if err := c.sleep(ctx, wait); err != nil {
			return exchange, &APIError{Message: "cancelled during backoff: " + err.Error()}
		}
	}
	return exchange, last
}

// once performs a single attempt. The returned duration is a server-requested
// wait, when one was given.
func (c *Client) once(ctx context.Context, body []byte) (Response, time.Duration, error) {
	attemptCtx, cancel := ctx, context.CancelFunc(func() {})
	if c.Timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, 0, &APIError{Message: "build request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		// Timeouts and connection failures are transient by nature.
		return Response{}, 0, &APIError{Message: redactKey(err.Error(), c.APIKey), Retryable: ctx.Err() == nil}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, 0, &APIError{Status: resp.StatusCode, Message: "read response: " + err.Error(), Retryable: true}
	}
	if len(raw) > maxResponseBytes {
		return Response{}, 0, &APIError{Status: resp.StatusCode, Message: "response exceeds the size bound"}
	}

	if resp.StatusCode != http.StatusOK {
		return Response{}, retryAfter(resp.Header.Get("Retry-After")), &APIError{
			Status:    resp.StatusCode,
			Message:   summarize(raw, c.APIKey),
			Retryable: retryableStatus(resp.StatusCode),
		}
	}

	var response Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return Response{}, 0, &APIError{Status: resp.StatusCode, Message: "decode response: " + err.Error()}
	}
	if response.Answers == nil {
		return Response{}, 0, &APIError{Status: resp.StatusCode, Message: "response carried no answers"}
	}
	return response, 0, nil
}

// retryableStatus: 429 and 529 ask the client to back off and retry, and a 5xx
// is a server fault worth another try. A 401 or 422 fails the same way every
// time.
func retryableStatus(status int) bool {
	switch {
	case status == http.StatusTooManyRequests, status == 529:
		return true
	case status >= 500 && status <= 599:
		return true
	default:
		return false
	}
}

// retryAfter parses a Retry-After header given in seconds. The HTTP-date form
// is ignored; exponential backoff covers it.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// pace enforces the minimum interval between requests.
func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	now := c.now()
	wait := time.Duration(0)
	if !c.lastRequest.IsZero() {
		if next := c.lastRequest.Add(c.MinInterval); next.After(now) {
			wait = next.Sub(now)
		}
	}
	c.lastRequest = now.Add(wait)
	c.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return c.sleep(ctx, wait)
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.BackoffBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	wait := base << attempt
	if wait <= 0 || wait > c.maxBackoff() {
		return c.maxBackoff()
	}
	return wait
}

func (c *Client) maxBackoff() time.Duration {
	if c.MaxBackoff <= 0 {
		return 10 * time.Second
	}
	return c.MaxBackoff
}

// httpClient preserves the caller's transport and timeout but refuses
// redirects: the key may not be forwarded to a different endpoint.
func (c *Client) httpClient() http.Client {
	base := c.HTTP
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// summarize shortens an error body for reporting, and strips the key in case a
// server echoed it back.
func summarize(raw []byte, key string) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	if text == "" {
		text = "empty response body"
	}
	return redactKey(text, key)
}

func redactKey(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}
