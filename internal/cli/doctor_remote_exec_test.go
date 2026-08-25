package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestRemoteExecDoctorCheck(t *testing.T) {
	t.Run("section absent", func(t *testing.T) {
		paths := writeRemoteExecDoctorConfig(t, t.TempDir(), "[daemon]\nworkers = 1\n")
		check, ok := remoteExecDoctorCheck(paths)
		if !ok || !check.OK || !check.Required {
			t.Fatalf("check = %#v, ok = %v, want required pass", check, ok)
		}
	})

	t.Run("config file absent", func(t *testing.T) {
		check, ok := remoteExecDoctorCheck(config.PathsForHome(t.TempDir()))
		if !ok || !check.OK || !check.Required {
			t.Fatalf("check = %#v, ok = %v, want required pass", check, ok)
		}
	})

	t.Run("valid section", func(t *testing.T) {
		paths := writeRemoteExecDoctorConfig(t, t.TempDir(), `[remote_exec]
backend = "local"
local_uid = 1
local_gid = 1
local_root = "/tmp/gitmoot-execbackend"
`)
		check, ok := remoteExecDoctorCheck(paths)
		if !ok || !check.OK || !check.Required {
			t.Fatalf("check = %#v, ok = %v, want required pass", check, ok)
		}
	})

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "uid without gid",
			body: "[remote_exec]\nlocal_uid = 1\n",
			want: "local_uid and [remote_exec].local_gid must be configured together",
		},
		{
			name: "relative local root",
			body: "[remote_exec]\nlocal_root = \"relative/instances\"\n",
			want: "local_root must be an absolute path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := writeRemoteExecDoctorConfig(t, t.TempDir(), tc.body)
			_, loaderErr := config.LoadRemoteExecConfig(paths)
			if loaderErr == nil {
				t.Fatal("loader accepted invalid remote exec fixture")
			}
			check, ok := remoteExecDoctorCheck(paths)
			if !ok || check.OK || !check.Required {
				t.Fatalf("check = %#v, ok = %v, want required failure", check, ok)
			}
			if !strings.Contains(check.Detail, tc.want) {
				t.Fatalf("detail = %q, want loader error containing %q", check.Detail, tc.want)
			}
			if !strings.Contains(check.Detail, loaderErr.Error()) {
				t.Fatalf("detail = %q, want exact loader error %q", check.Detail, loaderErr)
			}
		})
	}
}

func TestDoctorHomeSelectsRemoteExecConfig(t *testing.T) {
	ambientHome := t.TempDir()
	ambientPaths := config.PathsForHome(ambientHome)
	if err := os.MkdirAll(ambientPaths.ConfigFile, 0o700); err != nil {
		t.Fatalf("create ambient config trap: %v", err)
	}
	t.Setenv("HOME", ambientHome)
	// Make the live Claude probe fail unavailable without consulting host tools.
	t.Setenv("PATH", t.TempDir())

	explicitHome := t.TempDir()
	explicitPaths := writeRemoteExecDoctorConfig(t, explicitHome, `[remote_exec]
backend = "local"
local_uid = 1
local_gid = 1
local_root = "/tmp/gitmoot-explicit-home"
`)
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(explicitPaths.Home), map[string]string{
		runtime.ClaudeOAuthTokenEnv: testOAuthToken,
	}); err != nil {
		t.Fatalf("write explicit runtime auth: %v", err)
	}
	oldLookup, oldLogf := runtimeAuthEnvLookup, runtimeAuthLogf
	runtimeAuthEnvLookup = func(name string) (string, bool) {
		if name == runtime.AnthropicAPIKeyEnv {
			return testAPIKey, true
		}
		return "", false
	}
	runtimeAuthLogf = func(string, ...any) {}
	t.Cleanup(func() {
		runtimeAuthEnvLookup = oldLookup
		runtimeAuthLogf = oldLogf
	})

	var stdout, stderr bytes.Buffer
	Run([]string{"doctor", "--home", explicitHome, "--json", "--repo", t.TempDir()}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("doctor rejected --home: %s", stderr.String())
	}
	var checks []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &checks); err != nil {
		t.Fatalf("doctor --home JSON: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	t.Logf("doctor --home returned %d checks", len(checks))
	check, ok := doctorJSONCheckByName(checks, remoteExecDoctorCheckName)
	if !ok {
		t.Fatalf("doctor --home omitted %q check", remoteExecDoctorCheckName)
	}
	if got, _ := check["ok"].(bool); !got {
		t.Fatalf("doctor read ambient config instead of %s: %#v", explicitPaths.ConfigFile, check)
	}
	if detail, _ := check["detail"].(string); strings.Contains(detail, ambientPaths.ConfigFile) {
		t.Fatalf("doctor read ambient config %s: %q", ambientPaths.ConfigFile, detail)
	}
	authCheck, ok := doctorJSONCheckByName(checks, "claude auth")
	if !ok {
		t.Fatal("doctor --home omitted claude auth check")
	}
	authDetail, _ := authCheck["detail"].(string)
	for _, want := range []string{
		runtimeAuthFileName,
		runtime.ClaudeOAuthTokenEnv + "=set",
		runtime.AnthropicAPIKeyEnv + "=unset",
	} {
		if !strings.Contains(authDetail, want) {
			t.Fatalf("claude auth detail = %q, want %q from explicit home", authDetail, want)
		}
	}
	if _, err := os.Stat(runtimeAuthFilePath(ambientPaths.Home)); !os.IsNotExist(err) {
		t.Fatalf("doctor --home accessed ambient runtime auth at %s: %v", runtimeAuthFilePath(ambientPaths.Home), err)
	}
}

func writeRemoteExecDoctorConfig(t *testing.T, home, body string) config.Paths {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths
}
