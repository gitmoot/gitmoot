// Package jev calls TypeSafe's JEV judge model through OpenRouter's System One
// endpoint. Ported from github.com/gitmoot/workspace-janitor/internal/jev
// without the janitor-specific send gates and entry projections.
package jev

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	// DefaultEndpoint is OpenRouter's System One route.
	DefaultEndpoint = "https://openrouter.ai/api/v1/systemone"
	// DefaultModel is pinned so a model release never silently changes a
	// decision surface that was measured against a specific version.
	DefaultModel      = "typesafe/jev-1.13"
	DefaultTimeout    = 20 * time.Second
	DefaultMaxRetries = 2
)

// Request is the body of POST /api/v1/systemone: a state, a pinned model,
// and a map of typed questions.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Question is a typed question: noul (yes/no probability), choice, or score.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Encode renders a request exactly as it is sent. HTML escaping is off so
// diff text such as "<" reaches the model as written.
func Encode(request Request) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Response is the body of a successful evaluation.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Answer is one typed answer. Pointers distinguish a missing value from zero.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// Usage is the token accounting the API reports.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}
