package update

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestGhReleaseClientQueriesLatestRelease(t *testing.T) {
	runner := &fakeRunner{results: []subprocess.Result{{
		Stdout: `[
			{"tag_name":"v0.1.0-beta.1","html_url":"https://github.com/jerryfane/gitmoot/releases/tag/v0.1.0-beta.1","prerelease":true,"assets":[{"name":"gitmoot_linux_amd64","browser_download_url":"https://example.com/gitmoot","digest":"sha256:abc","size":12}]}
		]`,
	}}}
	client := GhReleaseClient{Runner: runner}

	release, err := client.LatestRelease(context.Background(), DefaultRepo)

	if err != nil {
		t.Fatalf("LatestRelease returned error: %v", err)
	}
	if release.TagName != "v0.1.0-beta.1" || len(release.Assets) != 1 {
		t.Fatalf("release = %+v", release)
	}
	runner.wantArgs(t, 0, "gh", "api", "repos/jerryfane/gitmoot/releases?per_page=20")
}

func TestGhReleaseClientMapsLatestReleaseNotFound(t *testing.T) {
	runner := &fakeRunner{
		results: []subprocess.Result{{Stderr: "HTTP 404: Not Found"}},
		errs:    []error{errors.New("exit status 1")},
	}
	client := GhReleaseClient{Runner: runner}

	_, err := client.LatestRelease(context.Background(), DefaultRepo)

	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("error = %v, want ErrNoRelease", err)
	}
}

func TestCheckReportsMissingRelease(t *testing.T) {
	client := fakeReleaseClient{err: ErrNoRelease}

	result, err := Check(context.Background(), client, DefaultRepo, buildinfo.Info{Version: "dev"}, "linux", "amd64", "")

	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.NoRelease || result.LatestVersion != "none" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCheckFindsPlatformAssetAndManualCommands(t *testing.T) {
	client := fakeReleaseClient{release: Release{
		TagName: "v0.1.0-beta.1",
		URL:     "https://github.com/jerryfane/gitmoot/releases/tag/v0.1.0-beta.1",
		Assets:  []Asset{{Name: "gitmoot_linux_amd64", Digest: "sha256:abc"}},
	}}

	result, err := Check(context.Background(), client, DefaultRepo, buildinfo.Info{Version: "v0.1.0-beta.0"}, "linux", "amd64", "/usr/local/bin/gitmoot")

	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if result.UpToDate {
		t.Fatal("result was up to date")
	}
	if result.Asset == nil || result.Asset.Name != "gitmoot_linux_amd64" {
		t.Fatalf("asset = %+v", result.Asset)
	}
	want := "gh release download v0.1.0-beta.1 --repo jerryfane/gitmoot --pattern gitmoot_linux_amd64 --output /tmp/gitmoot-update/gitmoot_linux_amd64"
	if !contains(result.ManualCommands, want) {
		t.Fatalf("manual commands = %+v, missing %q", result.ManualCommands, want)
	}
}

func TestManualCommandsKeepExecutableFallbackUsable(t *testing.T) {
	commands := ManualCommands(DefaultRepo, "v0.1.0-beta.1", &Asset{Name: "gitmoot_linux_amd64"}, "")

	want := "install -m 0755 /tmp/gitmoot-update/gitmoot_linux_amd64 $(command -v gitmoot)"
	if !contains(commands, want) {
		t.Fatalf("manual commands = %+v, missing %q", commands, want)
	}
}

func TestCheckMarksSameVersionUpToDate(t *testing.T) {
	client := fakeReleaseClient{release: Release{TagName: "v0.1.0-beta.1"}}

	result, err := Check(context.Background(), client, DefaultRepo, buildinfo.Info{Version: "0.1.0-beta.1"}, "linux", "amd64", "")

	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.UpToDate {
		t.Fatalf("result was not up to date: %+v", result)
	}
}

func TestApplyRefusesUnsafeDevelopmentBuild(t *testing.T) {
	result := CheckResult{
		CurrentVersion: "dev",
		LatestVersion:  "v0.1.0-beta.1",
		Asset:          &Asset{Name: "gitmoot_linux_amd64", Digest: "sha256:abc"},
	}
	runner := &fakeRunner{}

	applied, err := Apply(context.Background(), runner, DefaultRepo, result, "/usr/local/bin/gitmoot")

	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if applied.Applied || !strings.Contains(applied.Reason, "development builds") {
		t.Fatalf("applied = %+v", applied)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %+v, want none", runner.calls)
	}
}

func TestApplyRefusesMissingDigest(t *testing.T) {
	result := CheckResult{
		CurrentVersion: "v0.1.0-beta.0",
		LatestVersion:  "v0.1.0-beta.1",
		Asset:          &Asset{Name: "gitmoot_linux_amd64"},
	}

	applied, err := Apply(context.Background(), &fakeRunner{}, DefaultRepo, result, "/usr/local/bin/gitmoot")

	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if applied.Applied || !strings.Contains(applied.Reason, "sha256") {
		t.Fatalf("applied = %+v", applied)
	}
}

type fakeReleaseClient struct {
	release Release
	err     error
}

func (f fakeReleaseClient) LatestRelease(context.Context, string) (Release, error) {
	return f.release, f.err
}

type fakeRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *fakeRunner) LookPath(string) (string, error) {
	return "", errors.New("not found")
}

func (f *fakeRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	if !reflect.DeepEqual(f.calls[index], want) {
		t.Fatalf("call %d = %s\nwant %s", index, strings.Join(f.calls[index], " "), strings.Join(want, " "))
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
