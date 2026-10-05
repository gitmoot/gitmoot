package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

const fakeSandboxdAPIKey = "private-sandboxd-capacity-key"

// fakeSandboxd is a sandboxd gateway's control plane: its capacity report
// (GET /sandboxd/capacity), the E2B-shaped create, delete and list, and
// nothing else. Tests change its answers between provisions.
type fakeSandboxd struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// capacityStatus is the capacity endpoint's status; 0 serves capacity.
	capacityStatus int
	capacity       map[string]any
	// createStatus is POST /sandboxes's status; 0 creates a sandbox.
	createStatus  int
	capacityGets  int
	createPosts   int
	createdIDs    []string
	deleted       []string
	unexpectedReq []string
}

func newFakeSandboxd(t *testing.T) *fakeSandboxd {
	t.Helper()
	f := &fakeSandboxd{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.unexpectedReq) > 0 {
			t.Errorf("unexpected sandboxd requests: %v", f.unexpectedReq)
		}
	})
	return f
}

// reportCapacity makes the gateway report total slots across online workers
// and, per template, that template's slots.
func (f *fakeSandboxd) reportCapacity(total int, templates map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries := []map[string]any{}
	for id, slots := range templates {
		entries = append(entries, map[string]any{"templateID": id, "arch": "arm64", "totalSlots": slots, "freeSlots": slots})
	}
	f.capacityStatus = 0
	f.capacity = map[string]any{
		"totalSlots": total, "usedSlots": 0, "freeSlots": total, "templates": entries,
		"workers": []map[string]any{{"workerID": "worker-1", "online": true, "maxVMs": total}},
	}
}

func (f *fakeSandboxd) set(fn func(*fakeSandboxd)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSandboxd) counts() (capacityGets, createPosts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capacityGets, f.createPosts
}

func (f *fakeSandboxd) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := r.Header.Get("X-API-Key"); got != fakeSandboxdAPIKey {
		f.unexpectedReq = append(f.unexpectedReq, "bad X-API-Key on "+r.Method+" "+r.URL.Path)
		http.Error(w, `{"code":401,"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/sandboxd/capacity":
		f.capacityGets++
		if f.capacityStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.capacityStatus)
			_, _ = fmt.Fprintf(w, `{"code":%d,"message":"capacity status %d"}`, f.capacityStatus, f.capacityStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.capacity)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
		f.createPosts++
		var request struct {
			TemplateID string `json:"templateID"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = fmt.Fprintf(w, `{"code":%d,"message":"no free slot for template %s"}`, f.createStatus, request.TemplateID)
			return
		}
		id := fmt.Sprintf("sbx-%d", f.createPosts)
		f.createdIDs = append(f.createdIDs, id)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxID": id, "templateID": request.TemplateID, "clientID": "sandboxd",
			"envdAccessToken": "envd-token-" + id, "domain": nil,
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/sandboxes/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		f.unexpectedReq = append(f.unexpectedReq, r.Method+" "+r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

// sandboxdPlaceholderOrigin is the origin config.toml names. The section only
// accepts HTTPS origins, so sandboxdTestBackend points the loaded view at the
// fake's plain-HTTP listener, the one field a test cannot spell in config.
const sandboxdPlaceholderOrigin = "https://sandboxd.invalid:8443"

// configSection is a provider section for the fake gateway. ceiling 0 writes
// no max_concurrent.
func (f *fakeSandboxd) configSection(t *testing.T, section string, ceiling int) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "sandboxd-api-key")
	if err := os.WriteFile(keyFile, []byte(fakeSandboxdAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("\n[remote_exec.%s]\napi_key_file = %q\ntemplate = \"review-arm64\"\nbase_url = %q\nenvd_base_url = %q\n",
		section, keyFile, sandboxdPlaceholderOrigin, sandboxdPlaceholderOrigin)
	if ceiling != 0 {
		body += fmt.Sprintf("max_concurrent = %d\n", ceiling)
	}
	return body
}

// sandboxdCapacityTestHome writes a home whose default provider is E2B and
// which declares the gateway under [remote_exec.<section>].
func sandboxdCapacityTestHome(t *testing.T, f *fakeSandboxd, section string, ceiling int) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	e2bKey := filepath.Join(home, "e2b-api-key")
	if err := os.WriteFile(e2bKey, []byte("private-e2b-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("\n[remote_exec]\nbackend = \"local\"\ne2b_api_key_file = %q\ne2b_template = \"base\"\n", e2bKey) + f.configSection(t, section, ceiling)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return home
}

// sandboxdTestBackend constructs the execution backend the daemon would use
// for a job that opted into provider.
func sandboxdTestBackend(t *testing.T, f *fakeSandboxd, home, provider string) (execbackend.ExecutionBackend, *db.Store) {
	t.Helper()
	remote, err := config.LoadRemoteExecConfig(config.PathsForHome(home))
	if err != nil {
		t.Fatalf("load remote exec config: %v", err)
	}
	view, err := remote.ForProvider(provider)
	if err != nil {
		t.Fatalf("provider %q: %v", provider, err)
	}
	if view.E2BBaseURL != sandboxdPlaceholderOrigin {
		t.Fatalf("provider %q view base URL = %q; want the configured gateway %q", provider, view.E2BBaseURL, sandboxdPlaceholderOrigin)
	}
	view.E2BBaseURL, view.E2BEnvdBaseURL = f.server.URL, f.server.URL
	store := openExecBackendLedgerTestStore(t)
	worker := jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true, Stdout: io.Discard}
	backend, err := worker.defaultExecutionBackend(execbackend.Remote, view)
	if err != nil {
		t.Fatalf("construct %s execution backend: %v", provider, err)
	}
	return backend, store
}

// provisionUntilRefused provisions distinct jobs until one is refused (or
// limit succeed) and returns how many were admitted and the refusal.
func provisionUntilRefused(t *testing.T, backend execbackend.ExecutionBackend, limit int) (int, error) {
	t.Helper()
	for i := 1; i <= limit; i++ {
		instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: fmt.Sprintf("job-%d", i), LifecycleGeneration: 1, TTL: time.Minute})
		if err != nil {
			return i - 1, err
		}
		if instance == nil {
			t.Fatalf("provision job-%d returned no instance", i)
		}
	}
	return limit, nil
}

func requireCapRefusal(t *testing.T, err error, clause string) *db.ExecBackendCapRefusal {
	t.Helper()
	var refusal *db.ExecBackendCapRefusal
	if !errors.As(err, &refusal) || refusal.Clause != clause {
		t.Fatalf("refusal = %v; want a %q cap refusal", err, clause)
	}
	return refusal
}

// sandboxd reports how many sandboxes it can run. The provider's concurrency
// is that report, narrowed to the job's template, and max_concurrent is only
// an optional ceiling under it. The provision past the cap is refused before
// any create reaches sandboxd and writes no ledger row.
func TestSandboxdConcurrencyFollowsReportedCapacity(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		templates map[string]int
		ceiling   int
		want      int
	}{
		{name: "two reported slots, no ceiling", total: 2, templates: map[string]int{"review-arm64": 2}, want: 2},
		{name: "three reported slots, no ceiling", total: 3, templates: map[string]int{"review-arm64": 3}, want: 3},
		{name: "ceiling below the report caps it", total: 4, templates: map[string]int{"review-arm64": 4}, ceiling: 1, want: 1},
		{name: "ceiling above the report does not raise it", total: 2, templates: map[string]int{"review-arm64": 2}, ceiling: 5, want: 2},
		{name: "the job's template bounds below the cluster", total: 4, templates: map[string]int{"review-arm64": 2, "other": 2}, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := newFakeSandboxd(t)
			gateway.reportCapacity(test.total, test.templates)
			backend, store := sandboxdTestBackend(t, gateway, sandboxdCapacityTestHome(t, gateway, "sandboxd", test.ceiling), "sandboxd")

			admitted, err := provisionUntilRefused(t, backend, test.want+1)
			if admitted != test.want {
				t.Fatalf("admitted %d provisions (refusal %v); want %d", admitted, err, test.want)
			}
			requireCapRefusal(t, err, "concurrency")
			gets, posts := gateway.counts()
			if posts != test.want {
				t.Fatalf("sandboxd saw %d creates; want %d (none for the refused provision)", posts, test.want)
			}
			if gets == 0 {
				t.Fatal("admission never read GET /sandboxd/capacity")
			}
			refused, err := store.ListExecBackendAttemptsForJob(context.Background(), fmt.Sprintf("job-%d", test.want+1))
			if err != nil || len(refused) != 0 {
				t.Fatalf("refused provision ledger rows = %+v, %v; want none", refused, err)
			}
		})
	}
}

// A template no online worker serves, or a gateway whose report cannot be
// read, is a transient "capacity" refusal: nothing is created, and the
// review can wait for it to clear.
func TestSandboxdUnservedOrUnreadableCapacityRefusesWithoutCreating(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeSandboxd)
	}{
		{name: "template not served", setup: func(f *fakeSandboxd) { f.reportCapacity(3, map[string]int{"other": 3}) }},
		{name: "no online slot", setup: func(f *fakeSandboxd) { f.reportCapacity(0, map[string]int{}) }},
		{name: "capacity endpoint 503", setup: func(f *fakeSandboxd) {
			f.set(func(f *fakeSandboxd) { f.capacityStatus = http.StatusServiceUnavailable })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := newFakeSandboxd(t)
			test.setup(gateway)
			backend, _ := sandboxdTestBackend(t, gateway, sandboxdCapacityTestHome(t, gateway, "sandboxd", 0), "sandboxd")
			admitted, err := provisionUntilRefused(t, backend, 1)
			if admitted != 0 {
				t.Fatalf("admitted %d provisions; want the first refused", admitted)
			}
			requireCapRefusal(t, err, "capacity")
			if _, posts := gateway.counts(); posts != 0 {
				t.Fatalf("sandboxd saw %d creates; want none", posts)
			}
		})
	}
}

// A sandboxd older than the capacity endpoint answers 404. With
// max_concurrent set the provider keeps today's fixed cap; without it the
// first provision fails at once naming both fixes, since waiting never helps.
func TestSandboxdWithoutCapacityEndpointFallsBackToMaxConcurrent(t *testing.T) {
	t.Run("ceiling set", func(t *testing.T) {
		gateway := newFakeSandboxd(t)
		gateway.set(func(f *fakeSandboxd) { f.capacityStatus = http.StatusNotFound })
		backend, _ := sandboxdTestBackend(t, gateway, sandboxdCapacityTestHome(t, gateway, "sandboxd", 2), "sandboxd")
		admitted, err := provisionUntilRefused(t, backend, 3)
		if admitted != 2 {
			t.Fatalf("admitted %d provisions (refusal %v); want max_concurrent = 2", admitted, err)
		}
		requireCapRefusal(t, err, "concurrency")
		if _, posts := gateway.counts(); posts != 2 {
			t.Fatalf("sandboxd saw %d creates; want 2", posts)
		}
	})
	t.Run("no ceiling", func(t *testing.T) {
		gateway := newFakeSandboxd(t)
		gateway.set(func(f *fakeSandboxd) { f.capacityStatus = http.StatusNotFound })
		backend, _ := sandboxdTestBackend(t, gateway, sandboxdCapacityTestHome(t, gateway, "sandboxd", 0), "sandboxd")
		admitted, err := provisionUntilRefused(t, backend, 1)
		if admitted != 0 {
			t.Fatalf("admitted %d provisions; want a refusal", admitted)
		}
		refusal := requireCapRefusal(t, err, "unconfigured")
		if !strings.Contains(refusal.Error(), "upgrade sandboxd") || !strings.Contains(refusal.Error(), "max_concurrent") {
			t.Fatalf("refusal %q does not name both fixes (upgrade sandboxd, set max_concurrent)", refusal.Error())
		}
		if _, posts := gateway.counts(); posts != 0 {
			t.Fatalf("sandboxd saw %d creates; want none", posts)
		}
	})
}
