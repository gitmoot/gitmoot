package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestCheckHerdrVersionBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		output string
		wantOK bool
	}{
		{name: "delivery bug release", output: "herdr 0.7.4\n", wantOK: false},
		{name: "minimum", output: "herdr 0.7.5\n", wantOK: true},
		{name: "newer", output: "herdr version 1.0.0\n", wantOK: true},
		{name: "minimum prerelease", output: "herdr 0.7.5-rc.1\n", wantOK: false},
		{name: "malformed", output: "herdr v0.7.5\n", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := fakeRunner{paths: map[string]bool{"herdr": true}, runs: map[string]subprocess.Result{"herdr --version": {Stdout: test.output}}}
			check := CheckHerdrVersion(context.Background(), runner, OrgMinimumHerdrVersion)
			if check.OK != test.wantOK || !check.Required {
				t.Fatalf("check = %+v, want OK=%v required", check, test.wantOK)
			}
			if !test.wantOK && !strings.Contains(check.Detail, "org requires herdr >=0.7.5") {
				t.Fatalf("detail = %q", check.Detail)
			}
		})
	}
}

func TestCheckHerdrVersionCommandFailures(t *testing.T) {
	missing := CheckHerdrVersion(context.Background(), fakeRunner{}, OrgMinimumHerdrVersion)
	if missing.OK || !strings.Contains(missing.Detail, "install herdr") {
		t.Fatalf("missing check = %+v", missing)
	}
	runner := fakeRunner{paths: map[string]bool{"herdr": true}, errs: map[string]error{"herdr --version": errors.New("boom")}}
	failed := CheckHerdrVersion(context.Background(), runner, OrgMinimumHerdrVersion)
	if failed.OK || !strings.Contains(failed.Detail, "--version` failed") {
		t.Fatalf("failed check = %+v", failed)
	}
}

func TestGlobalChecksGatesHerdrOnlyWhenOrgEnabled(t *testing.T) {
	writeConfig := func(t *testing.T, body string) config.Paths {
		t.Helper()
		paths := config.PathsForHome(t.TempDir())
		if err := os.MkdirAll(paths.Home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return paths
	}
	runner := fakeRunner{paths: map[string]bool{"herdr": true}, runs: map[string]subprocess.Result{"herdr --version": {Stdout: "herdr 0.7.5\n"}}}
	for _, test := range []struct {
		name      string
		body      string
		wantHerdr bool
		wantOrg   bool
	}{
		{name: "absent", body: "[workflow]\nresult_checks = \"warn\"\n"},
		{name: "enabled", body: "[org]\nenforce = \"warn\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\nmerge_rule = \"owner\"\n", wantHerdr: true},
		{name: "malformed", body: "[org]\nenforce = \"invalid\"\n", wantOrg: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := (Checker{Runner: runner, Paths: writeConfig(t, test.body), SkipDaemonAuth: true}).GlobalChecks(context.Background())
			var foundHerdr, foundOrg bool
			for _, check := range checks {
				foundHerdr = foundHerdr || check.Name == "herdr"
				foundOrg = foundOrg || check.Name == "org registry"
			}
			if foundHerdr != test.wantHerdr || foundOrg != test.wantOrg {
				t.Fatalf("checks = %+v", checks)
			}
		})
	}
}

func TestGlobalChecksMissingConfigDoesNotEnableOrg(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	checks := (Checker{Runner: fakeRunner{}, Paths: paths, SkipDaemonAuth: true}).GlobalChecks(context.Background())
	for _, check := range checks {
		if check.Name == "herdr" || check.Name == "org registry" {
			t.Fatalf("absent org config added check: %+v", check)
		}
	}
}
