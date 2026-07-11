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

// Flow is the ownership-relevant projection returned by GetFlow.
type Flow struct {
	ID          string         `json:"id"`
	DisplayName string         `json:"displayName"`
	Status      string         `json:"status"`
	Metadata    map[string]any `json:"metadata,omitempty"`
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
	// Activepieces 0.82 rejects sign-up on an already-provisioned platform with
	// 403 INVITATION_ONLY_SIGN_UP (not 409), so treat it as "account exists" and
	// fall through to sign-in — every post-setup session hits this path.
	return httpErr.StatusCode == http.StatusConflict || strings.Contains(body, "already exist") || strings.Contains(body, "user exist") || strings.Contains(body, "invitation only sign up")
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
	err := c.UpsertPieceConnection(ctx, token, projectID, externalID, "Gitmoot Bridge", "@gitmoot/piece-gitmoot", map[string]any{
		"bridge_url": bridgeURL, "bridge_token": bridgeToken,
	}, recreate)
	if err != nil {
		return fmt.Errorf("create Activepieces bridge connection: %w", err)
	}
	return nil
}

// UpsertPieceConnection creates a project-scoped CUSTOM_AUTH connection. When
// recreate is true, an external-id conflict is resolved by deleting the exact
// matching connection and creating it again (which re-runs piece validation).
func (c *Client) UpsertPieceConnection(ctx context.Context, token, projectID, externalID, displayName, pieceName string, props map[string]any, recreate bool) error {
	body := map[string]any{
		"externalId": externalID, "displayName": displayName, "projectId": projectID,
		"scope": "PROJECT", "pieceName": pieceName, "type": "CUSTOM_AUTH",
		"value": map[string]any{"type": "CUSTOM_AUTH", "props": props},
	}
	path := "/api/v1/app-connections"
	if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
		if !resourceAlreadyExists(err) {
			return err
		}
		if !recreate {
			return nil
		}
		connectionID, findErr := c.findConnectionID(ctx, token, projectID, externalID)
		if findErr != nil {
			return fmt.Errorf("find existing Activepieces connection: %w", findErr)
		}
		if connectionID == "" {
			return errors.New("Activepieces reported an existing connection but did not return it from the connection list")
		}
		deletePath := "/api/v1/app-connections/" + url.PathEscape(connectionID)
		if err := c.doJSON(ctx, http.MethodDelete, deletePath, token, nil, nil); err != nil {
			return fmt.Errorf("delete Activepieces connection: %w", err)
		}
		if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
			return fmt.Errorf("recreate Activepieces connection: %w", err)
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

func (c *Client) PublishFlow(ctx context.Context, token, flowID string) error {
	return c.flowOperation(ctx, token, flowID, "LOCK_AND_PUBLISH", map[string]any{})
}

func (c *Client) SetFlowStatus(ctx context.Context, token, flowID string, enabled bool) error {
	status := "DISABLED"
	if enabled {
		status = "ENABLED"
	}
	return c.flowOperation(ctx, token, flowID, "CHANGE_STATUS", map[string]any{"status": status})
}

func (c *Client) UpdateFlowMetadata(ctx context.Context, token, flowID string, metadata map[string]any) error {
	return c.flowOperation(ctx, token, flowID, "UPDATE_METADATA", map[string]any{"metadata": metadata})
}

func (c *Client) flowOperation(ctx context.Context, token, flowID, operation string, request map[string]any) error {
	body := map[string]any{"type": operation, "request": request}
	path := "/api/v1/flows/" + url.PathEscape(flowID)
	if err := c.doJSON(ctx, http.MethodPost, path, token, body, nil); err != nil {
		return fmt.Errorf("Activepieces flow operation %s: %w", operation, err)
	}
	return nil
}

func (c *Client) GetFlow(ctx context.Context, token, flowID string) (Flow, error) {
	path := "/api/v1/flows/" + url.PathEscape(flowID)
	var response struct {
		ID          string         `json:"id"`
		DisplayName string         `json:"displayName"`
		Status      string         `json:"status"`
		Metadata    map[string]any `json:"metadata"`
		Version     struct {
			DisplayName string         `json:"displayName"`
			Metadata    map[string]any `json:"metadata"`
		} `json:"version"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
		return Flow{}, fmt.Errorf("get Activepieces flow: %w", err)
	}
	metadata := response.Metadata
	if metadata == nil {
		metadata = response.Version.Metadata
	}
	// AP 0.82 serves the display name nested under version.displayName on GET
	// /v1/flows/{id}; a top-level displayName is not part of that response.
	displayName := strings.TrimSpace(response.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(response.Version.DisplayName)
	}
	return Flow{ID: response.ID, DisplayName: displayName, Status: response.Status, Metadata: metadata}, nil
}

// ResolvePieceVersion asks Activepieces for the installed metadata matching the
// requested range. AP 0.82 exposes this as GET /api/v1/pieces/:pieceName with
// version and projectId query parameters.
func (c *Client) ResolvePieceVersion(ctx context.Context, token, projectID, pieceName, requestedVersion string) (string, error) {
	query := url.Values{"projectId": {projectID}}
	if strings.TrimSpace(requestedVersion) != "" {
		query.Set("version", requestedVersion)
	}
	path := pieceMetadataPath(pieceName) + "?" + query.Encode()
	var response struct {
		Version      string `json:"version"`
		PieceVersion string `json:"pieceVersion"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
		return "", fmt.Errorf("resolve Activepieces piece %s: %w", pieceName, err)
	}
	version := strings.TrimSpace(response.Version)
	if version == "" {
		version = strings.TrimSpace(response.PieceVersion)
	}
	if version == "" {
		return "", fmt.Errorf("resolve Activepieces piece %s: response is missing version", pieceName)
	}
	return strings.TrimLeft(version, "~^"), nil
}

func pieceMetadataPath(pieceName string) string {
	pieceName = strings.TrimSpace(pieceName)
	if scope, name, ok := strings.Cut(pieceName, "/"); ok {
		// AP 0.82 has a distinct /:scope/:name route for scoped pieces; escaping
		// the slash as part of one parameter does not match that route.
		return "/api/v1/pieces/" + url.PathEscape(scope) + "/" + url.PathEscape(name)
	}
	return "/api/v1/pieces/" + url.PathEscape(pieceName)
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
