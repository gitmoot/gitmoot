// Package hubclient calls a gitmoot hub (#2710) with this machine's client
// certificate. It trusts only the hub's own authority (ca.pem), never the
// system roots, and speaks TLS 1.3 only.
package hubclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RoleHeader is the header naming the acting role; it matches hub.RoleHeader.
const RoleHeader = "X-Gitmoot-Role"

const maxResponseBytes = 4 << 20

// Client is a hub connection for one machine.
type Client struct {
	base string
	http *http.Client
	leaf *x509.Certificate
}

// New loads ca.pem, NAME.crt and NAME.key from certs and returns a client for
// the hub at base (https://HOST:PORT).
func New(base, certs, name string) (*Client, error) {
	caPEM, err := os.ReadFile(filepath.Join(certs, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("hub client: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("hub client: %s holds no certificate", filepath.Join(certs, "ca.pem"))
	}
	keyPath := filepath.Join(certs, name+".key")
	info, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("hub client: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("hub client: %s is readable by others (mode %v); chmod 600 it", keyPath, info.Mode().Perm())
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(certs, name+".crt"), keyPath)
	if err != nil {
		return nil, fmt.Errorf("hub client: load certificate: %w", err)
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               nil, // the hub is a Tailscale peer; never route it through a proxy
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("hub client: parse certificate: %w", err)
	}
	return &Client{base: strings.TrimSuffix(base, "/"), http: &http.Client{Transport: transport, Timeout: 4 * time.Minute}, leaf: leaf}, nil
}

// Error is a refusal from the hub: its HTTP status and {"error"} text.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("hub: %s (HTTP %d)", e.Message, e.Status) }

// Do sends in (JSON, or no body when nil) to path as role, and decodes a 2xx
// JSON answer into out when out is not nil. A non-2xx answer is an *Error.
// An empty role lets the hub use the machine's only role.
func (c *Client) Do(ctx context.Context, method, path, role string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if role = strings.TrimSpace(role); role != "" {
		request.Header.Set(RoleHeader, role)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("hub: read answer: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("hub: answer too large")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &refusal) != nil || refusal.Error == "" {
			refusal.Error = strings.TrimSpace(string(data))
		}
		return &Error{Status: response.StatusCode, Message: refusal.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("hub: decode answer: %w", err)
	}
	return nil
}

// Base is the hub's https origin.
func (c *Client) Base() string { return c.base }

// Certificate is this machine's client certificate.
func (c *Client) Certificate() *x509.Certificate { return c.leaf }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }
