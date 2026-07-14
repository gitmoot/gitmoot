package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
)

func seedTriggerPipeline(t *testing.T, home string, raw []byte, enabled bool, binding pipelineTriggerBinding) {
	t.Helper()
	if err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{
			Name: "mail-flow", Repo: "owner/repo", SpecYAML: string(raw), SpecHash: pipeline.Hash(raw), Enabled: enabled,
		}); err != nil {
			return err
		}
		if binding.BindingID == "" && binding.FlowID == "" && binding.State == "" {
			return nil
		}
		encoded, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		return store.SetPipelineTriggerBinding(ctx, "mail-flow", string(encoded))
	}); err != nil {
		t.Fatal(err)
	}
}

func writeTriggerTestAdminCredentials(t *testing.T, home, password string) {
	t.Helper()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	path := activepiecesCredentialsPath(paths.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Email="+defaultActivepiecesEmail+"\nPassword="+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func triggerRecord(t *testing.T, home string) db.Pipeline {
	return triggerRecordNamed(t, home, "mail-flow")
}

func triggerRecordNamed(t *testing.T, home, name string) db.Pipeline {
	t.Helper()
	var rec db.Pipeline
	if err := withStore(home, func(store *db.Store) error {
		var ok bool
		var err error
		rec, ok, err = store.GetPipeline(context.Background(), name)
		if err == nil && !ok {
			t.Fatalf("%s not found", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestBindPipelineTriggerPublishesAndRefusesOwnershipMismatch(t *testing.T) {
	home := t.TempDir()
	raw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo ok}\n")
	if _, err := pipeline.Load(raw); err != nil {
		t.Fatal(err)
	}
	var operations []string
	mismatch := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
			_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/pieces/"):
			if strings.Contains(r.URL.Path, "piece-imap") {
				_, _ = w.Write([]byte(`{"version":"0.4.3"}`))
			} else {
				_, _ = w.Write([]byte(`{"version":"0.1.3"}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"id":"flow-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/flow-1":
			name := "gitmoot: mail-flow"
			metadata := `{"gitmoot":{"binding_id":"other"}}`
			if mismatch {
				name = "manual flow"
			}
			_, _ = w.Write([]byte(`{"id":"flow-1","displayName":"` + name + `","status":"ENABLED","metadata":` + metadata + `}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/flow-1":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			operations = append(operations, body["type"].(string))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	if err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "mail-flow", Repo: "owner/repo", SpecYAML: string(raw), SpecHash: pipeline.Hash(raw)}); err != nil {
			return err
		}
		rec, _, err := store.GetPipeline(ctx, "mail-flow")
		if err != nil {
			return err
		}
		binding, err := bindPipelineTrigger(ctx, store, rec, activepiecesAuthOptions{Home: home, URL: server.URL, Password: "admin"}, triggerBindingError)
		if err != nil {
			return err
		}
		if binding.State != triggerBindingBound || binding.FlowID != "flow-1" || binding.PieceVersions["imap"] != "0.4.3" || binding.PieceVersions["gitmoot"] != "0.1.3" {
			t.Fatalf("binding = %+v", binding)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"IMPORT_FLOW", "UPDATE_METADATA", "LOCK_AND_PUBLISH"}; strings.Join(operations, ",") != strings.Join(want, ",") {
		t.Fatalf("operations = %v, want %v", operations, want)
	}

	mismatch = true
	before := len(operations)
	if err := withStore(home, func(store *db.Store) error {
		rec, _, err := store.GetPipeline(context.Background(), "mail-flow")
		if err != nil {
			return err
		}
		_, err = bindPipelineTrigger(context.Background(), store, rec, activepiecesAuthOptions{Home: home, URL: server.URL, Password: "admin"}, triggerBindingError)
		if err == nil || !strings.Contains(err.Error(), "refusing to modify") {
			t.Fatalf("ownership error = %v", err)
		}
		updated, _, getErr := store.GetPipeline(context.Background(), "mail-flow")
		if getErr != nil {
			return getErr
		}
		binding, decodeErr := decodeTriggerBinding(updated.TriggerBinding)
		if decodeErr != nil || binding.State != triggerBindingError {
			t.Fatalf("error binding = %+v err=%v", binding, decodeErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(operations) != before {
		t.Fatalf("ownership mismatch mutated flow: operations=%v", operations)
	}
}

func TestBindMappedPipelineRequiresGitmootPiece014(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    string
		resolveErr bool
	}{
		{name: "old version", version: "0.1.3"},
		{name: "resolution failure", resolveErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			raw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\n  map: {subject: subject}\nstages:\n  - {id: run, cmd: echo ok}\n")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
					_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "piece-imap"):
					_, _ = w.Write([]byte(`{"version":"0.4.3"}`))
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "piece-gitmoot"):
					if tc.resolveErr {
						http.Error(w, "missing", http.StatusNotFound)
						return
					}
					_, _ = w.Write([]byte(`{"version":"` + tc.version + `"}`))
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()

			if err := withStore(home, func(store *db.Store) error {
				ctx := context.Background()
				if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "mail-flow", Repo: "owner/repo", SpecYAML: string(raw), SpecHash: pipeline.Hash(raw)}); err != nil {
					return err
				}
				rec, _, err := store.GetPipeline(ctx, "mail-flow")
				if err != nil {
					return err
				}
				_, err = bindPipelineTrigger(ctx, store, rec, activepiecesAuthOptions{Home: home, URL: server.URL, Password: "admin"}, triggerBindingPending)
				if err == nil || !strings.Contains(err.Error(), "@gitmoot/piece-gitmoot >= 0.1.4") {
					t.Fatalf("bind error = %v", err)
				}
				updated, _, getErr := store.GetPipeline(ctx, "mail-flow")
				if getErr != nil {
					return getErr
				}
				binding, decodeErr := decodeTriggerBinding(updated.TriggerBinding)
				if decodeErr != nil || binding.State != triggerBindingError || !strings.Contains(binding.LastError, "0.1.4") {
					t.Fatalf("binding = %+v decodeErr=%v", binding, decodeErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPieceVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.1.3", false}, {"0.1.4-beta.1", false}, {"0.1.4", true}, {"v0.1.4+build", true}, {"0.2.0-beta.1", true}, {"1.0.0", true}, {"bad", false},
	} {
		if got := pieceVersionAtLeast(tc.version, "0.1.4"); got != tc.want {
			t.Errorf("pieceVersionAtLeast(%q)=%v want %v", tc.version, got, tc.want)
		}
	}
}

func TestPipelineDisableAndRemoveRemainLocalFirstWhenAPUnavailable(t *testing.T) {
	for _, command := range []string{"disable", "remove"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			raw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo ok}\n")
			bindingJSON, err := json.Marshal(pipelineTriggerBinding{
				FlowID: "flow-manual-cleanup", BindingID: "binding-1", BaseURL: "http://127.0.0.1:1",
				ProjectID: "project-1", State: triggerBindingBound,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := withStore(home, func(store *db.Store) error {
				return store.CreateOrUpdatePipeline(context.Background(), db.Pipeline{
					Name: "mail-flow", Repo: "owner/repo", SpecYAML: string(raw), SpecHash: pipeline.Hash(raw),
					Enabled: true, TriggerBinding: string(bindingJSON),
				})
			}); err != nil {
				t.Fatal(err)
			}
			// CreateOrUpdate does not write trigger_binding on insert by design; use
			// the dedicated atomic setter just as the binder does.
			if err := withStore(home, func(store *db.Store) error {
				return store.SetPipelineTriggerBinding(context.Background(), "mail-flow", string(bindingJSON))
			}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := Run([]string{"pipeline", command, "mail-flow", "--home", home}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("%s exit=%d stderr=%s", command, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "flow-manual-cleanup") && command == "remove" {
				t.Fatalf("remove warning lacks flow id: %s", stderr.String())
			}
			if err := withStore(home, func(store *db.Store) error {
				rec, ok, err := store.GetPipeline(context.Background(), "mail-flow")
				if err != nil {
					return err
				}
				if command == "remove" {
					if ok {
						t.Fatalf("pipeline still exists after local-first remove: %+v", rec)
					}
					return nil
				}
				if !ok || rec.Enabled {
					t.Fatalf("pipeline was not disabled locally: ok=%v rec=%+v", ok, rec)
				}
				binding, decodeErr := decodeTriggerBinding(rec.TriggerBinding)
				if decodeErr != nil || binding.State != triggerBindingError {
					t.Fatalf("disable binding=%+v err=%v", binding, decodeErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPipelineReAddWithoutTriggerCleansStaleFlowOrKeepsBindingOnFailure(t *testing.T) {
	oldRaw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo old}\n")
	newRaw := "name: mail-flow\nrepo: owner/repo\nstages:\n  - {id: run, cmd: echo new}\n"

	t.Run("cleanup succeeds", func(t *testing.T) {
		deleted := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
				_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/stale-flow":
				_, _ = w.Write([]byte(`{"id":"stale-flow","displayName":"gitmoot: mail-flow","status":"ENABLED"}`))
			case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/flows/stale-flow":
				deleted++
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected request", http.StatusNotFound)
			}
		}))
		defer server.Close()
		home := t.TempDir()
		writeTriggerTestAdminCredentials(t, home, "admin")
		seedTriggerPipeline(t, home, oldRaw, true, pipelineTriggerBinding{FlowID: "stale-flow", BindingID: "bind-1", BaseURL: server.URL, ProjectID: "project-1", State: triggerBindingBound})
		specFile := writeSpec(t, newRaw)
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("add exit=%d stderr=%s", code, stderr.String())
		}
		if deleted != 1 || triggerRecord(t, home).TriggerBinding != "" || !strings.Contains(stdout.String(), "cleaned up stale trigger flow") {
			t.Fatalf("deleted=%d binding=%q stdout=%s stderr=%s", deleted, triggerRecord(t, home).TriggerBinding, stdout.String(), stderr.String())
		}
	})

	t.Run("cleanup failure keeps binding", func(t *testing.T) {
		home := t.TempDir()
		writeTriggerTestAdminCredentials(t, home, "admin")
		seedTriggerPipeline(t, home, oldRaw, true, pipelineTriggerBinding{FlowID: "stale-flow", BindingID: "bind-1", BaseURL: "http://127.0.0.1:1", ProjectID: "project-1", State: triggerBindingBound})
		specFile := writeSpec(t, newRaw)
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "add", specFile, "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("add exit=%d stderr=%s", code, stderr.String())
		}
		if triggerRecord(t, home).TriggerBinding == "" || !strings.Contains(stderr.String(), "gitmoot pipeline bind-trigger mail-flow") {
			t.Fatalf("binding=%q stderr=%s", triggerRecord(t, home).TriggerBinding, stderr.String())
		}
	})
}

func TestBindTriggerCleansBindingWhenSpecNoLongerHasTrigger(t *testing.T) {
	deleted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
			_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/stale-flow":
			_, _ = w.Write([]byte(`{"id":"stale-flow","displayName":"gitmoot: mail-flow"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/flows/stale-flow":
			deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	raw := []byte("name: mail-flow\nrepo: owner/repo\nstages:\n  - {id: run, cmd: echo}\n")
	seedTriggerPipeline(t, home, raw, true, pipelineTriggerBinding{FlowID: "stale-flow", BindingID: "bind-1", BaseURL: server.URL, State: triggerBindingBound})
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "bind-trigger", "mail-flow", "--home", home, "--password", "admin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bind-trigger exit=%d stderr=%s", code, stderr.String())
	}
	if deleted != 1 || triggerRecord(t, home).TriggerBinding != "" || !strings.Contains(stdout.String(), "cleaned up stale trigger flow") {
		t.Fatalf("deleted=%d binding=%q stdout=%s", deleted, triggerRecord(t, home).TriggerBinding, stdout.String())
	}
}

func TestBindTriggerWithoutSpecOrBindingStillErrors(t *testing.T) {
	home := t.TempDir()
	raw := []byte("name: mail-flow\nrepo: owner/repo\nstages:\n  - {id: run, cmd: echo}\n")
	seedTriggerPipeline(t, home, raw, true, pipelineTriggerBinding{})
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "bind-trigger", "mail-flow", "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "has no trigger block") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestBindPipelineTriggerRecreatesDeletedFlow(t *testing.T) {
	created := 0
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
			_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/pieces/"):
			_, _ = w.Write([]byte(`{"version":"0.1.3"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/deleted-flow":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows":
			created++
			_, _ = w.Write([]byte(`{"id":"new-flow"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/new-flow":
			_, _ = w.Write([]byte(`{"id":"new-flow","displayName":"gitmoot: mail-flow"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/new-flow":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			operations = append(operations, body["type"].(string))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	raw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo}\n")
	seedTriggerPipeline(t, home, raw, true, pipelineTriggerBinding{FlowID: "deleted-flow", BindingID: "bind-1", BaseURL: server.URL, State: triggerBindingBound})
	if err := withStore(home, func(store *db.Store) error {
		rec, _, err := store.GetPipeline(context.Background(), "mail-flow")
		if err != nil {
			return err
		}
		_, err = bindPipelineTrigger(context.Background(), store, rec, activepiecesAuthOptions{Home: home, URL: server.URL, Password: "admin"}, triggerBindingError)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := decodeTriggerBinding(triggerRecord(t, home).TriggerBinding)
	if err != nil || binding.FlowID != "new-flow" || binding.State != triggerBindingBound || created != 1 {
		t.Fatalf("binding=%+v created=%d err=%v", binding, created, err)
	}
	if got, want := strings.Join(operations, ","), "IMPORT_FLOW,UPDATE_METADATA,LOCK_AND_PUBLISH"; got != want {
		t.Fatalf("operations=%s want=%s", got, want)
	}
}

func TestPipelineEnableBindFailurePreservesEnabledState(t *testing.T) {
	raw := []byte("name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo}\n")
	for _, originallyEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[originallyEnabled], func(t *testing.T) {
			home := t.TempDir()
			seedTriggerPipeline(t, home, raw, originallyEnabled, pipelineTriggerBinding{})
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"pipeline", "enable", "mail-flow", "--home", home}, &stdout, &stderr); code == 0 {
				t.Fatalf("enable unexpectedly succeeded: stdout=%s", stdout.String())
			}
			if got := triggerRecord(t, home).Enabled; got != originallyEnabled {
				t.Fatalf("enabled=%v, want original %v; stderr=%s", got, originallyEnabled, stderr.String())
			}
		})
	}
}

func TestPipelineDisableCorruptSpecIsUnconditionalAndAttemptsBoundFlow(t *testing.T) {
	t.Run("no binding", func(t *testing.T) {
		home := t.TempDir()
		seedTriggerPipeline(t, home, []byte("not: [valid"), true, pipelineTriggerBinding{})
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "disable", "mail-flow", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("disable exit=%d stderr=%s", code, stderr.String())
		}
		if triggerRecord(t, home).Enabled {
			t.Fatal("corrupt-spec pipeline remained enabled")
		}
	})

	t.Run("binding is disabled remotely", func(t *testing.T) {
		statusCalls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
				_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/bound-flow":
				_, _ = w.Write([]byte(`{"id":"bound-flow","displayName":"gitmoot: mail-flow"}`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/bound-flow":
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["type"] == "CHANGE_STATUS" {
					statusCalls++
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected", http.StatusNotFound)
			}
		}))
		defer server.Close()
		home := t.TempDir()
		seedTriggerPipeline(t, home, []byte("not: [valid"), true, pipelineTriggerBinding{FlowID: "bound-flow", BindingID: "bind-1", BaseURL: server.URL, State: triggerBindingBound})
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "disable", "mail-flow", "--home", home, "--password", "admin"}, &stdout, &stderr); code != 0 {
			t.Fatalf("disable exit=%d stderr=%s", code, stderr.String())
		}
		if triggerRecord(t, home).Enabled || statusCalls != 1 {
			t.Fatalf("enabled=%v statusCalls=%d stderr=%s", triggerRecord(t, home).Enabled, statusCalls, stderr.String())
		}
	})
}

func TestPipelineEnableCorruptSpecFallsBackOnlyWithoutBinding(t *testing.T) {
	t.Run("legacy no binding", func(t *testing.T) {
		home := t.TempDir()
		seedTriggerPipeline(t, home, []byte("not: [valid"), false, pipelineTriggerBinding{})
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "enable", "mail-flow", "--home", home}, &stdout, &stderr); code != 0 {
			t.Fatalf("enable exit=%d stderr=%s", code, stderr.String())
		}
		if !triggerRecord(t, home).Enabled {
			t.Fatal("legacy pipeline was not enabled")
		}
	})

	t.Run("binding requires parseable spec", func(t *testing.T) {
		home := t.TempDir()
		seedTriggerPipeline(t, home, []byte("not: [valid"), false, pipelineTriggerBinding{FlowID: "bound", BindingID: "bind-1", State: triggerBindingBound})
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"pipeline", "enable", "mail-flow", "--home", home}, &stdout, &stderr); code == 0 {
			t.Fatalf("enable unexpectedly succeeded: %s", stdout.String())
		}
		if triggerRecord(t, home).Enabled {
			t.Fatal("corrupt bound pipeline changed enabled state")
		}
	})
}

func TestOpenActivepiecesSessionUsesStackFrontendURL(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/authentication/sign-up" {
			http.NotFound(w, r)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
	}))
	defer server.Close()
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(paths.Home, "activepieces")
	if err := os.MkdirAll(stackDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, ".env"), []byte("AP_FRONTEND_URL="+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := openActivepiecesSession(context.Background(), activepiecesAuthOptions{Home: home, Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || session.Client.BaseURL() != server.URL {
		t.Fatalf("hits=%d baseURL=%s", hits, session.Client.BaseURL())
	}
}

func TestPipelineAddAutoBindUsesStackFrontendURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
			_, _ = w.Write([]byte(`{"token":"token","projectId":"project-1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/pieces/"):
			_, _ = w.Write([]byte(`{"version":"0.1.3"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"id":"auto-flow"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows/auto-flow":
			_, _ = w.Write([]byte(`{"id":"auto-flow","displayName":"gitmoot: mail-flow"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/auto-flow":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	writeTriggerTestAdminCredentials(t, home, "admin")
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(paths.Home, "activepieces")
	if err := os.WriteFile(filepath.Join(stackDir, ".env"), []byte("AP_FRONTEND_URL="+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specFile := writeSpec(t, "name: mail-flow\nrepo: owner/repo\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo}\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--enable", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, stderr.String())
	}
	binding, err := decodeTriggerBinding(triggerRecord(t, home).TriggerBinding)
	if err != nil || binding.State != triggerBindingBound || binding.FlowID != "auto-flow" || binding.BaseURL != server.URL {
		t.Fatalf("binding=%+v err=%v stderr=%s", binding, err, stderr.String())
	}
}

func TestPipelineTriggerKindNeverTouchesActivepieces(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "pipeline triggers must not call Activepieces", http.StatusInternalServerError)
	}))
	defer server.Close()
	home := t.TempDir()
	writeTriggerTestAdminCredentials(t, home, "admin")
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "activepieces", ".env"), []byte("AP_FRONTEND_URL="+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specFile := writeSpec(t, "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages: [{id: run, cmd: echo}]\n")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "add", specFile, "--enable", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, stderr.String())
	}
	if hits != 0 || strings.Contains(stderr.String(), "Activepieces") || strings.Contains(stderr.String(), "trigger is pending") {
		t.Fatalf("add touched Activepieces: hits=%d stderr=%s", hits, stderr.String())
	}
	if binding := triggerRecordNamed(t, home, "downstream").TriggerBinding; binding != "" {
		t.Fatalf("pipeline trigger wrote binding %q", binding)
	}
	if code := Run([]string{"pipeline", "disable", "downstream", "--home", home, "--url", server.URL, "--password", "admin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, stderr.String())
	}
	if code := Run([]string{"pipeline", "enable", "downstream", "--home", home, "--url", server.URL, "--password", "admin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"pipeline", "bind-trigger", "downstream", "--home", home, "--url", server.URL, "--password", "admin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bind-trigger exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no Activepieces binding is needed") {
		t.Fatalf("bind-trigger message = %q", stdout.String())
	}
	if hits != 0 || triggerRecordNamed(t, home, "downstream").TriggerBinding != "" {
		t.Fatalf("pipeline trigger path touched Activepieces: hits=%d binding=%q", hits, triggerRecordNamed(t, home, "downstream").TriggerBinding)
	}
}
