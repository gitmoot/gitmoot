package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/hub"
)

// hubTestServer serves hubAPI exactly as startHub does, but on loopback with
// loopback admitted as a peer, since a test host has no Tailscale address.
func hubTestServer(t *testing.T, home string) string {
	t.Helper()
	paths := config.PathsForHome(home)
	authority, err := hub.LoadOrCreateAuthority(hub.Dir(paths.Home))
	if err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := hub.Authenticate(store.ActiveHubMachineByFingerprint, time.Now,
		[]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, hubAPI(paths, store))
	server, err := hub.Serve(listener, authority, []string{"127.0.0.1"}, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return "https://" + server.Addr().String()
}

// hubTestClient trusts only ca.pem and presents NAME.pem/NAME.key from dir,
// the three files `hub machine add` wrote.
func hubTestClient(t *testing.T, dir, name string) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem holds no certificate")
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func hubCall(t *testing.T, client *http.Client, method, url, role, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		request.Header.Set(hub.RoleHeader, role)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func hubCLI(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(append(append([]string{"hub"}, args...), "--home", home), &out, &diag)
	return code, out.String(), diag.String()
}

func TestHubMachineSendsAndRepliesAsItsRoleOverMutualTLS(t *testing.T) {
	home := messageTestHome(t)
	certs := filepath.Join(t.TempDir(), "mac")
	if code, _, diag := hubCLI(t, home, "machine", "add", "mac-studio", "--role", "gm-omp-nag", "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	if info, err := os.Stat(filepath.Join(certs, "mac-studio.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("client key mode: %v %v", info, err)
	}
	base := hubTestServer(t, home)
	client := hubTestClient(t, certs, "mac-studio")

	status, body := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"role": "gm-omp-nag"`) || !strings.Contains(body, `"machine": "mac-studio"`) {
		t.Fatalf("whoami %d %s", status, body)
	}

	status, body = hubCall(t, client, http.MethodPost, base+"/v1/messages", "", `{"to":"deimos","body":"from the mac"}`)
	if status != http.StatusCreated {
		t.Fatalf("send %d %s", status, body)
	}
	var sent messageTestResult
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Sender != "gm-omp-nag" || sent.Recipient != "deimos" || sent.Body != "from the mac" {
		t.Fatalf("sent=%+v", sent)
	}

	// The local recipient sees it in the ordinary inbox and replies locally.
	var inbox []messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "inbox"), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != sent.ID {
		t.Fatalf("deimos inbox=%+v", inbox)
	}
	var replied messageTestResult
	if err := json.Unmarshal(messageCLI(t, home, "deimos", "reply", fmt.Sprint(sent.ID), "seen"), &replied); err != nil {
		t.Fatal(err)
	}

	// The remote machine reads the reply through the hub and answers it.
	status, body = hubCall(t, client, http.MethodGet, base+"/v1/messages", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"message": "seen"`) {
		t.Fatalf("remote inbox %d %s", status, body)
	}
	status, body = hubCall(t, client, http.MethodPost, fmt.Sprintf("%s/v1/messages/%d/reply", base, replied.ID), "", `{"body":"thanks"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"to": "deimos"`) {
		t.Fatalf("remote reply %d %s", status, body)
	}

	// Both writes are recorded against the machine and role.
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var writes int
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(*) FROM hub_writes w JOIN hub_machines m ON m.id = w.machine_id WHERE m.name = 'mac-studio' AND w.role = 'gm-omp-nag'`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatalf("hub_writes=%d, want 2", writes)
	}
}

func TestHubRefusesOtherRolesUnregisteredAndRevokedMachines(t *testing.T) {
	home := messageTestHome(t)
	certs := filepath.Join(t.TempDir(), "air")
	if code, _, diag := hubCLI(t, home, "machine", "add", "air", "--role", "gm-omp-nag", "--role", "deimos", "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	base := hubTestServer(t, home)
	client := hubTestClient(t, certs, "air")

	// Two roles: the request must name one, and only one of those two.
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "", ""); status != http.StatusBadRequest {
		t.Fatalf("no role header: %d %s", status, body)
	}
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/messages", "jarvis", `{"to":"owner","body":"x"}`); status != http.StatusForbidden {
		t.Fatalf("role outside the machine's list: %d %s", status, body)
	}
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "deimos", ""); status != http.StatusOK || !strings.Contains(body, `"role": "deimos"`) {
		t.Fatalf("named role: %d %s", status, body)
	}

	// Revoking stops the certificate on the next request of the same
	// keep-alive connection.
	if code, _, diag := hubCLI(t, home, "machine", "revoke", "air"); code != 0 {
		t.Fatalf("revoke exit=%d: %s", code, diag)
	}
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "deimos", ""); status != http.StatusForbidden {
		t.Fatalf("revoked machine: %d %s", status, body)
	}

	// A new certificate under the same name works; the old one stays refused.
	fresh := filepath.Join(t.TempDir(), "air2")
	if code, _, diag := hubCLI(t, home, "machine", "add", "air", "--role", "deimos", "--out", fresh); code != 0 {
		t.Fatalf("re-add exit=%d: %s", code, diag)
	}
	if status, _ := hubCall(t, hubTestClient(t, fresh, "air"), http.MethodGet, base+"/v1/whoami", "", ""); status != http.StatusOK {
		t.Fatalf("new certificate: %d", status)
	}
	if status, _ := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "deimos", ""); status != http.StatusForbidden {
		t.Fatalf("old certificate after re-add: %d", status)
	}
}

func TestHubMachineAddRefusesUnknownRoleAndNonEmptyOut(t *testing.T) {
	home := messageTestHome(t)
	if code, _, diag := hubCLI(t, home, "machine", "add", "pi", "--role", "nobody", "--out", filepath.Join(t.TempDir(), "x")); code != 1 || !strings.Contains(diag, `unknown role "nobody"`) {
		t.Fatalf("unknown role: exit=%d %s", code, diag)
	}
	busy := t.TempDir()
	if err := os.WriteFile(filepath.Join(busy, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, diag := hubCLI(t, home, "machine", "add", "pi", "--role", "deimos", "--out", busy); code != 1 || !strings.Contains(diag, "is not empty") {
		t.Fatalf("non-empty out: exit=%d %s", code, diag)
	}
	if code, out, _ := hubCLI(t, home, "machine", "ls"); code != 0 || !strings.Contains(out, "No machines.") {
		t.Fatalf("a refused add registered a machine: %s", out)
	}
}

// A role removed from the org stops working remotely at once, without
// revoking the machine or restarting the hub.
func TestHubRefusesARoleNoLongerInTheOrg(t *testing.T) {
	home := messageTestHome(t)
	certs := filepath.Join(t.TempDir(), "pi")
	if code, _, diag := hubCLI(t, home, "machine", "add", "pi", "--role", "gm-omp-impl", "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	base := hubTestServer(t, home)
	client := hubTestClient(t, certs, "pi")
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/messages", "", ""); status != http.StatusOK {
		t.Fatalf("before removal: %d %s", status, body)
	}
	configFile := config.PathsForHome(home).ConfigFile
	content, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.Replace(string(content), "[org.roles.\"gm-omp-impl\"]\nparent=\"gitmoot\"\nscope=[\"gitmoot/implementation\"]\n", "", 1)
	if trimmed == string(content) {
		t.Fatal("test config no longer holds the gm-omp-impl block this test removes")
	}
	if err := os.WriteFile(configFile, []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/messages", "", ""); status != http.StatusForbidden {
		t.Fatalf("after removal: %d %s", status, body)
	}
}

// Every valid machine name enrolls, including one that matches the CA file's
// stem, and its certificate works.
func TestHubMachineNamedCAEnrolls(t *testing.T) {
	home := messageTestHome(t)
	certs := filepath.Join(t.TempDir(), "ca-machine")
	if code, _, diag := hubCLI(t, home, "machine", "add", "ca", "--role", "deimos", "--out", certs); code != 0 {
		t.Fatalf("machine add ca exit=%d: %s", code, diag)
	}
	base := hubTestServer(t, home)
	if status, body := hubCall(t, hubTestClient(t, certs, "ca"), http.MethodGet, base+"/v1/whoami", "", ""); status != http.StatusOK || !strings.Contains(body, `"machine": "ca"`) {
		t.Fatalf("whoami as ca: %d %s", status, body)
	}
}
