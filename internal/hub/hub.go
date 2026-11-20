// Package hub serves gitmoot's API to clients on other machines (#2710).
//
// The hub listens on one Tailscale address with TLS 1.3 and requires a client
// certificate from the hub's own certificate authority, which is kept under
// <home>/hub/ca. A client certificate identifies a machine; the hub_machines
// table maps it to the org roles that machine may act as. Each request names
// one of those roles, or the machine's only one. Revoking a machine takes
// effect on its next request, including on a connection already open,
// because every request looks the certificate up again.
package hub

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/mtlsca"
)

// RoleHeader names the org role a request acts as.
const RoleHeader = "X-Gitmoot-Role"

const (
	authorityValidity = 10 * 365 * 24 * time.Hour
	serverValidity    = 365 * 24 * time.Hour
	authorityDir      = "ca"
	authorityCertFile = "ca.pem"
	authorityKeyFile  = "ca.key"
)

// Caller is the authenticated machine and the role the request acts as.
type Caller struct {
	Machine db.HubMachine
	Role    string
}

type callerKey struct{}

// CallerFrom returns the caller Authenticate attached to the request context.
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}

// Dir is the hub's directory under a resolved gitmoot home.
func Dir(home string) string { return filepath.Join(home, "hub") }

// LoadOrCreateAuthority loads the hub authority from <hubDir>/ca, creating it
// on first use. Creation writes both files into a private temporary directory
// and renames it into place, so two processes racing to create it end with
// one authority and neither sees half of one.
func LoadOrCreateAuthority(hubDir string) (*mtlsca.Authority, error) {
	if err := os.MkdirAll(hubDir, 0o700); err != nil {
		return nil, fmt.Errorf("hub directory: %w", err)
	}
	target := filepath.Join(hubDir, authorityDir)
	if authority, err := loadAuthority(target); err == nil || !errors.Is(err, os.ErrNotExist) {
		return authority, err
	}
	authority, err := mtlsca.New("gitmoot hub", authorityValidity)
	if err != nil {
		return nil, err
	}
	keyPEM, err := authority.PrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(hubDir, authorityDir+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("hub authority staging: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.WriteFile(filepath.Join(staging, authorityKeyFile), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write hub authority key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, authorityCertFile), authority.CertificatePEM(), 0o644); err != nil {
		return nil, fmt.Errorf("write hub authority certificate: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		// Another process created it first; use theirs.
		if existing, loadErr := loadAuthority(target); loadErr == nil {
			return existing, nil
		}
		return nil, fmt.Errorf("install hub authority: %w", err)
	}
	return authority, nil
}

func loadAuthority(dir string) (*mtlsca.Authority, error) {
	certificatePEM, err := os.ReadFile(filepath.Join(dir, authorityCertFile))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(dir, authorityKeyFile))
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("hub authority key %s is readable by others (mode %v); chmod 600 it", filepath.Join(dir, authorityKeyFile), info.Mode().Perm())
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, authorityKeyFile))
	if err != nil {
		return nil, err
	}
	authority, err := mtlsca.Parse(certificatePEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("hub authority in %s: %w", dir, err)
	}
	return authority, nil
}

// Fingerprint is the lowercase hex SHA-256 of a certificate's DER bytes.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// MachineLookup finds the active machine for a certificate fingerprint, or
// returns sql.ErrNoRows.
type MachineLookup func(ctx context.Context, fingerprint string, now time.Time) (db.HubMachine, error)

// Authenticate admits a request only from a peer address inside peers
// (config.TailscalePrefixes in production), with a client certificate that
// belongs to an unrevoked, unexpired machine, acting as one of that machine's
// roles. It attaches the Caller to the context.
func Authenticate(lookup MachineLookup, now func() time.Time, peers []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !peerAllowed(r.RemoteAddr, peers) {
			WriteError(w, http.StatusForbidden, "the hub serves Tailscale peers only")
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			WriteError(w, http.StatusUnauthorized, "a client certificate is required")
			return
		}
		machine, err := lookup(r.Context(), Fingerprint(r.TLS.PeerCertificates[0].Raw), now())
		if errors.Is(err, sql.ErrNoRows) {
			WriteError(w, http.StatusForbidden, "this certificate is not registered, or its machine was revoked")
			return
		}
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "machine lookup failed")
			return
		}
		role := strings.ToLower(strings.TrimSpace(r.Header.Get(RoleHeader)))
		switch {
		case role == "" && len(machine.Roles) == 1:
			role = machine.Roles[0]
		case role == "":
			WriteError(w, http.StatusBadRequest, fmt.Sprintf("machine %s may act as %s; name one in %s", machine.Name, strings.Join(machine.Roles, ", "), RoleHeader))
			return
		case !slices.Contains(machine.Roles, role):
			WriteError(w, http.StatusForbidden, fmt.Sprintf("machine %s may not act as %s", machine.Name, role))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, Caller{Machine: machine, Role: role})))
	})
}

func peerAllowed(remoteAddr string, peers []netip.Prefix) bool {
	address, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := address.Addr().Unmap()
	for _, prefix := range peers {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// Server is a running hub listener.
type Server struct {
	server   *http.Server
	listener net.Listener
}

// Serve starts the hub on listener, presenting a server certificate for hosts
// signed by authority and accepting client certificates from it only. The
// handler sees authenticated requests only. Serve returns once the listener
// is serving; Close stops it.
func Serve(listener net.Listener, authority *mtlsca.Authority, hosts []string, handler http.Handler) (*Server, error) {
	serverCertificate, err := authority.IssueServer(hosts, serverValidity)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    authority.Pool(),
	})
	server := &http.Server{
		Handler:           handler,
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() { _ = server.Serve(tlsListener) }()
	return &Server{server: server, listener: tlsListener}, nil
}

// Addr is the address the hub listens on.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Close stops the hub, waiting up to three seconds for requests in flight.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}

// WriteError writes a JSON error body {"error": message}.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
