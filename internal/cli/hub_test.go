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
	"net/http/httptest"
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
// Every endpoint group is allowed; hubTestServerAllow takes a [hub] allow.
func hubTestServer(t *testing.T, home string) string {
	t.Helper()
	return hubTestServerAllow(t, home, config.HubGroups)
}

func hubTestServerAllow(t *testing.T, home string, allow []string) string {
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
		[]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, hubAllow(allow, hubAPI(home, paths, store)))
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
	if n := hubWriteCount(t, home, "message.send") + hubWriteCount(t, home, "message.reply"); n != 2 {
		t.Fatalf("hub_writes=%d, want 2", n)
	}
}

// hubWriteCount counts hub_writes rows for action, from any machine.
func hubWriteCount(t *testing.T, home, action string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var writes int
	if err := raw.QueryRowContext(context.Background(),
		`SELECT count(*) FROM hub_writes w JOIN hub_machines m ON m.id = w.machine_id WHERE w.action = ? AND w.role IN (SELECT role FROM hub_machine_roles WHERE machine_id = m.id)`, action).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	return writes
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

// With the default [hub] allow a machine can message and claim its own mail,
// and nothing else: org, tasks and reviews are refused before any handler
// runs, whoami reports the groups, and no path spelling routes around the
// check (#2710, owner decision: remote machines are messages only).
func TestHubDefaultAllowServesMessagingOnly(t *testing.T) {
	home := messageTestHome(t)
	certs := filepath.Join(t.TempDir(), "mac")
	if code, _, diag := hubCLI(t, home, "machine", "add", "mac", "--role", "gm-omp-impl", "--out", certs); code != 0 {
		t.Fatalf("machine add exit=%d: %s", code, diag)
	}
	base := hubTestServerAllow(t, home, config.DefaultHubAllow)
	client := hubTestClient(t, certs, "mac")

	status, body := hubCall(t, client, http.MethodGet, base+"/v1/whoami", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"allow": [`) || !strings.Contains(body, `"messages"`) || !strings.Contains(body, `"notifications"`) || strings.Contains(body, `"reviews"`) {
		t.Fatalf("whoami %d %s", status, body)
	}
	if status, body := hubCall(t, client, http.MethodPost, base+"/v1/messages", "", `{"to":"deimos","body":"allowed"}`); status != http.StatusCreated {
		t.Fatalf("messages refused: %d %s", status, body)
	}
	if status, body := hubCall(t, client, http.MethodGet, base+"/v1/messages", "", ""); status != http.StatusOK {
		t.Fatalf("inbox refused: %d %s", status, body)
	}
	for _, call := range []struct{ method, path, body, group string }{
		{http.MethodGet, "/v1/org/brief", "", "org"},
		{http.MethodGet, "/v1/tasks?ref=7&repo=gitmoot/implementation", "", "tasks"},
		{http.MethodPost, "/v1/reviews/level", `{"repo":"gitmoot/implementation","pr":7}`, "reviews"},
		{http.MethodPost, "/v1/reviews", `{"repo":"gitmoot/implementation","pr":7,"message":"m"}`, "reviews"},
	} {
		status, body := hubCall(t, client, call.method, base+call.path, "", call.body)
		if status != http.StatusForbidden || !strings.Contains(body, "does not serve "+call.group) {
			t.Errorf("%s %s: %d %s", call.method, call.path, status, body)
		}
	}
	for _, sneaky := range []string{"/v1/messages/../reviews/level", "/v1/messages/%2e%2e/reviews/level", "/v1//reviews/level", "/v1/messages%2F..%2Freviews%2Flevel"} {
		status, body := hubCall(t, client, http.MethodPost, base+sneaky, "", `{"repo":"gitmoot/implementation","pr":7}`)
		if status == http.StatusOK || status == http.StatusCreated || strings.Contains(body, `"decision"`) {
			t.Errorf("%s reached a handler: %d %s", sneaky, status, body)
		}
	}
	if status, _ := hubCall(t, client, http.MethodGet, base+"/v1/unknown", "", ""); status != http.StatusNotFound {
		t.Errorf("a path in no group: %d", status)
	}
	if n := hubWriteCount(t, home, "review.level") + hubWriteCount(t, home, "review.request"); n != 0 {
		t.Fatalf("a refused review endpoint recorded %d writes", n)
	}
}

// A route added to the hub later without a [hub] allow group is refused, even
// with every group allowed and a handler registered for it: the check fails
// closed instead of passing paths it does not know.
func TestHubAllowRefusesARouteInNoGroup(t *testing.T) {
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/config", func(http.ResponseWriter, *http.Request) { reached = true })
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(hubAllowedFrom(r.Context()))
	})
	handler := hubAllow(config.HubGroups, mux)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil))
	if reached || recorder.Code != http.StatusNotFound {
		t.Fatalf("a route in no group: reached=%t status=%d", reached, recorder.Code)
	}
	// whoami needs no group, even with nothing allowed.
	recorder = httptest.NewRecorder()
	hubAllow([]string{}, mux).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/whoami", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("whoami with allow = []: %d %q", recorder.Code, recorder.Body.String())
	}
}

// An unclean or escaped path is refused before routing, so a router that
// does not normalise paths itself can never be handed one group's path that
// the check classified as another (#2710 [hub] allow).
func TestHubAllowRefusesUncleanPathsBeforeRouting(t *testing.T) {
	// %20, %25, %3F and %2F are the default escapes Go records with an empty
	// URL.RawPath, the review's probes; they must be refused like the rest.
	for _, target := range []string{"/v1/messages/../reviews/level", "/v1/messages/./x", "/v1/messages%2F..%2Freviews",
		"/v1/messages/a%20b", "/v1/messages/%25", "/v1/messages/%3F", "/v1/tasks/%207"} {
		reached := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		recorder := httptest.NewRecorder()
		hubAllow(config.DefaultHubAllow, next).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if reached || recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: reached=%t status=%d", target, reached, recorder.Code)
		}
	}
}
