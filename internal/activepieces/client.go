package activepieces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type FlowSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	detail := strings.TrimSpace(e.Body)
	if detail == "" {
		return fmt.Sprintf("Activepieces %s %s returned %s", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("Activepieces %s %s returned %s: %s", e.Method, e.Path, e.Status, detail)
}

func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Activepieces URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("invalid Activepieces URL scheme %q", parsed.Scheme)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}, nil
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) SignUpOrIn(ctx context.Context, email, password string) (token, projectID string, created bool, err error) {
	signUp := map[string]any{
		"email":       strings.TrimSpace(email),
		"password":    password,
		"firstName":   "Gitmoot",
		"lastName":    "Admin",
		"trackEvents": false,
		"newsLetter":  false,
	}
	var auth authenticationResponse
	err = c.doJSON(ctx, http.MethodPost, "/api/v1/authentication/sign-up", "", signUp, &auth)
	if err == nil {
		token, projectID, err = auth.values()
		if err != nil {
			return "", "", false, err
		}
		return token, projectID, true, nil
	}
	if !accountAlreadyExists(err) {
		return "", "", false, fmt.Errorf("sign up to Activepieces: %w", err)
	}

	auth = authenticationResponse{}
	signIn := map[string]string{"email": strings.TrimSpace(email), "password": password}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/authentication/sign-in", "", signIn, &auth); err != nil {
		return "", "", false, fmt.Errorf("sign in to Activepieces: %w", err)
	}
	token, projectID, err = auth.values()
	if err != nil {
		return "", "", false, err
	}
	return token, projectID, false, nil
}

type authenticationResponse struct {
	Token     string `json:"token"`
	ProjectID string `json:"projectId"`
	Project   struct {
		ID string `json:"id"`
	} `json:"project"`
}

func (r authenticationResponse) values() (string, string, error) {
	projectID := strings.TrimSpace(r.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(r.Project.ID)
	}
	if strings.TrimSpace(r.Token) == "" || projectID == "" {
		return "", "", errors.New("Activepieces authentication response is missing token or projectId")
	}
	return strings.TrimSpace(r.Token), projectID, nil
}

func accountAlreadyExists(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	body := normalizedErrorBody(httpErr.Body)
	return httpErr.StatusCode == http.StatusConflict || strings.Contains(body, "already exist") || strings.Contains(body, "user exist")
}

func (c *Client) InstallPiece(ctx context.Context, token, pieceName, pieceVersion string) error {
	pieceVersion = strings.TrimSpace(pieceVersion)
	if pieceVersion == "" {
		// Activepieces 0.82 requires an exact pieceVersion for a REGISTRY
		// install; an empty value is rejected with an opaque 400.
		return errors.New("install Activepieces piece: an exact piece version is required")
	}
	body := map[string]any{
		"pieceName":    pieceName,
		"pieceVersion": pieceVersion,
		"scope":        "PLATFORM",
		"packageType":  "REGISTRY",
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/pieces", token, body, nil); err != nil {
		if resourceAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("install Activepieces piece: %w", err)
	}
	return nil
}

func resourceAlreadyExists(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	body := normalizedErrorBody(httpErr.Body)
	return httpErr.StatusCode == http.StatusConflict || strings.Contains(body, "already installed") || strings.Contains(body, "already exist")
}

func normalizedErrorBody(body string) string {
	body = strings.ToLower(body)
	return strings.NewReplacer("_", " ", "-", " ").Replace(body)
}

func (c *Client) UpsertBridgeConnection(ctx context.Context, token, projectID, externalID, bridgeURL, bridgeToken string, recreate bool) error {
	if strings.TrimSpace(externalID) == "" {
		externalID = "gitmoot-bridge"
	}
	body := map[string]any{
		"externalId":  externalID,
		"displayName": "Gitmoot Bridge",
		"projectId":   projectID,
		"scope":       "PROJECT",
		"pieceName":   "@gitmoot/piece-gitmoot",
		"type":        "CUSTOM_AUTH",
		"value": map[string]any{
			"type": "CUSTOM_AUTH",
			"props": map[string]string{
				"bridge_url":   bridgeURL,
				"bridge_token": bridgeToken,
			},
		},
	}
	path := "/api/v1/app-connections"
	if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
		if !resourceAlreadyExists(err) {
			return fmt.Errorf("create Activepieces bridge connection: %w", err)
		}
		if !recreate {
			return nil
		}
		connectionID, findErr := c.findConnectionID(ctx, token, projectID, externalID)
		if findErr != nil {
			return fmt.Errorf("find existing Activepieces bridge connection: %w", findErr)
		}
		if connectionID == "" {
			return errors.New("Activepieces reported an existing bridge connection but did not return it from the connection list")
		}
		deletePath := "/api/v1/app-connections/" + url.PathEscape(connectionID)
		if err := c.doJSON(ctx, http.MethodDelete, deletePath, token, nil, nil); err != nil {
			return fmt.Errorf("delete Activepieces bridge connection: %w", err)
		}
		if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
			return fmt.Errorf("recreate Activepieces bridge connection: %w", err)
		}
	}
	return nil
}

func (c *Client) findConnectionID(ctx context.Context, token, projectID, externalID string) (string, error) {
	query := url.Values{"projectId": {projectID}}
	path := "/api/v1/app-connections?" + query.Encode()
	var response struct {
		Data []struct {
			ID         string `json:"id"`
			ExternalID string `json:"externalId"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
		return "", err
	}
	for _, connection := range response.Data {
		if connection.ExternalID == externalID {
			return connection.ID, nil
		}
	}
	return "", nil
}

func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/flags", nil)
		if err != nil {
			return err
		}
		resp, err := c.httpClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("Activepieces health endpoint returned %s", resp.Status)
		} else {
			lastErr = err
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for Activepieces health: %w (last response: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func (c *Client) CreateFlow(ctx context.Context, token, projectID, displayName string) (string, error) {
	body := map[string]string{"displayName": displayName, "projectId": projectID}
	var response struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/flows", token, body, &response); err != nil {
		return "", fmt.Errorf("create Activepieces flow: %w", err)
	}
	if strings.TrimSpace(response.ID) == "" {
		return "", errors.New("create Activepieces flow: response is missing id")
	}
	return response.ID, nil
}

func (c *Client) ImportFlow(ctx context.Context, token, flowID string, flow json.RawMessage) error {
	var request json.RawMessage
	if err := json.Unmarshal(flow, &request); err != nil {
		return fmt.Errorf("decode Activepieces flow template: %w", err)
	}
	body := struct {
		Type    string          `json:"type"`
		Request json.RawMessage `json:"request"`
	}{Type: "IMPORT_FLOW", Request: request}
	path := "/api/v1/flows/" + url.PathEscape(flowID)
	if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
		return fmt.Errorf("import Activepieces flow: %w", err)
	}
	return nil
}

func (c *Client) DeleteFlow(ctx context.Context, token, flowID string) error {
	path := "/api/v1/flows/" + url.PathEscape(flowID)
	if err := c.doJSON(ctx, http.MethodDelete, path, token, nil, nil); err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete Activepieces flow: %w", err)
	}
	return nil
}

func (c *Client) ListFlows(ctx context.Context, token, projectID string) ([]FlowSummary, error) {
	query := url.Values{"projectId": {projectID}}
	path := "/api/v1/flows?" + query.Encode()
	var response struct {
		Data []FlowSummary `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
		return nil, fmt.Errorf("list Activepieces flows: %w", err)
	}
	return response.Data, nil
}

func (c *Client) doJSON(ctx context.Context, method, path, token string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Method: method, Path: path, StatusCode: resp.StatusCode, Status: resp.Status, Body: string(raw)}
	}
	if responseBody != nil && len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, responseBody); err != nil {
			return fmt.Errorf("decode Activepieces response: %w", err)
		}
	}
	return nil
}
