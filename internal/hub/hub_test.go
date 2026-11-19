package hub

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/mtlsca"
)

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

type testHub struct {
	url       string
	authority *mtlsca.Authority
	machines  map[string]db.HubMachine // fingerprint -> machine
}

func startTestHub(t *testing.T, peers []netip.Prefix) *testHub {
	t.Helper()
	authority, err := LoadOrCreateAuthority(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &testHub{authority: authority, machines: map[string]db.HubMachine{}}
	lookup := func(_ context.Context, fingerprint string, _ time.Time) (db.HubMachine, error) {
		machine, ok := h.machines[fingerprint]
		if !ok {
			return db.HubMachine{}, sql.ErrNoRows
		}
		return machine, nil
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, _ := CallerFrom(r.Context())
		_, _ = w.Write([]byte(caller.Machine.Name + "/" + caller.Role))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := Serve(listener, authority, []string{"127.0.0.1"}, Authenticate(lookup, time.Now, peers, ok))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	h.url = "https://" + server.Addr().String() + "/"
	return h
}

func (h *testHub) client(t *testing.T, issuer *mtlsca.Authority, name string, register bool, roles ...string) *http.Client {
	t.Helper()
	leaf, err := issuer.IssueClient(name, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if register {
		h.machines[hex.EncodeToString(leaf.Fingerprint[:])] = db.HubMachine{Name: name, Roles: roles}
	}
	certificate, err := tls.X509KeyPair(leaf.CertificatePEM, leaf.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return h.clientWith(t, []tls.Certificate{certificate})
}

// clientWith presents its certificate whatever CAs the server asks for. Go's
// own client would withhold a certificate from another authority, which hides
// whether the server verifies the chain; an attacker's client sends it anyway.
func (h *testHub) clientWith(t *testing.T, certificates []tls.Certificate) *http.Client {
	present := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		if len(certificates) == 0 {
			return &tls.Certificate{}, nil
		}
		return &certificates[0], nil
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: h.authority.Pool(), GetClientCertificate: present, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func get(t *testing.T, client *http.Client, url string, role string) (int, string, error) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		request.Header.Set(RoleHeader, role)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	var body strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, readErr := response.Body.Read(buffer)
		body.Write(buffer[:n])
		if readErr != nil {
			break
		}
	}
	return response.StatusCode, body.String(), nil
}

func TestHubAdmitsOnlyRegisteredCertificatesFromItsAuthority(t *testing.T) {
	h := startTestHub(t, loopback)

	status, body, err := get(t, h.client(t, h.authority, "mac", true, "deimos"), h.url, "")
	if err != nil || status != http.StatusOK || body != "mac/deimos" {
		t.Fatalf("registered machine: %d %q %v", status, body, err)
	}

	// No client certificate: the TLS handshake itself fails.
	if _, _, err := get(t, h.clientWith(t, nil), h.url, ""); err == nil {
		t.Fatal("a client without a certificate was served")
	}

	// A certificate from another authority, even one registered by
	// fingerprint, fails the handshake.
	foreign, err := mtlsca.New("someone else", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := get(t, h.client(t, foreign, "mac", true, "deimos"), h.url, ""); err == nil {
		t.Fatal("a certificate from a foreign authority was served")
	}

	// The hub's own authority, but no machine row: refused.
	status, _, err = get(t, h.client(t, h.authority, "stray", false), h.url, "")
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("unregistered certificate: %d %v", status, err)
	}
}

func TestHubRoleMustBeOneOfTheMachines(t *testing.T) {
	h := startTestHub(t, loopback)
	client := h.client(t, h.authority, "air", true, "deimos", "gm-omp-nag")
	if status, _, _ := get(t, client, h.url, ""); status != http.StatusBadRequest {
		t.Fatalf("two roles, none named: %d", status)
	}
	if status, _, _ := get(t, client, h.url, "jarvis"); status != http.StatusForbidden {
		t.Fatalf("role outside the list: %d", status)
	}
	if status, body, _ := get(t, client, h.url, "GM-OMP-NAG"); status != http.StatusOK || body != "air/gm-omp-nag" {
		t.Fatalf("named role: %d %q", status, body)
	}
}

func TestHubRefusesPeersOutsideTheAllowedNetwork(t *testing.T) {
	// The production prefixes are Tailscale's; a loopback peer is outside.
	h := startTestHub(t, []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")})
	status, _, err := get(t, h.client(t, h.authority, "mac", true, "deimos"), h.url, "")
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("loopback peer: %d %v", status, err)
	}
}

func TestAuthorityPersistsAndRefusesAnExposedKey(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateAuthority(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.CertificatePEM()) != string(second.CertificatePEM()) {
		t.Fatal("a second load created a new authority; issued certificates would stop working")
	}
	if err := os.Chmod(filepath.Join(dir, authorityDir, authorityKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateAuthority(dir); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("world-readable key: %v", err)
	}
}
