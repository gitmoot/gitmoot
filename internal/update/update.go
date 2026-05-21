package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const DefaultRepo = "jerryfane/gitmoot"

var ErrNoRelease = errors.New("no GitHub release found")

type ReleaseClient interface {
	LatestRelease(ctx context.Context, repo string) (Release, error)
}

type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	URL        string  `json:"html_url"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
	Size               int64  `json:"size"`
}

type GhReleaseClient struct {
	Runner subprocess.Runner
	Dir    string
}

func (c GhReleaseClient) LatestRelease(ctx context.Context, repo string) (Release, error) {
	runner := c.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	result, err := runner.Run(ctx, c.Dir, "gh", "api", "repos/"+repo+"/releases?per_page=20")
	if err != nil {
		if isNotFound(result) {
			return Release{}, ErrNoRelease
		}
		return Release{}, commandError(result, err)
	}
	var releases []Release
	if err := json.Unmarshal([]byte(result.Stdout), &releases); err != nil {
		return Release{}, fmt.Errorf("decode releases: %w", err)
	}
	for _, release := range releases {
		if !release.Draft {
			return release, nil
		}
	}
	return Release{}, ErrNoRelease
}

type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	ReleaseURL     string
	UpToDate       bool
	NoRelease      bool
	Asset          *Asset
	ManualCommands []string
}

func Check(ctx context.Context, client ReleaseClient, repo string, current buildinfo.Info, goos string, goarch string, executable string) (CheckResult, error) {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	release, err := client.LatestRelease(ctx, repo)
	if errors.Is(err, ErrNoRelease) {
		return CheckResult{
			CurrentVersion: current.Version,
			LatestVersion:  "none",
			NoRelease:      true,
			ManualCommands: []string{"gh release view --repo " + shellQuote(repo)},
		}, nil
	}
	if err != nil {
		return CheckResult{}, err
	}
	asset := matchingAsset(release.Assets, goos, goarch)
	result := CheckResult{
		CurrentVersion: current.Version,
		LatestVersion:  release.TagName,
		ReleaseURL:     release.URL,
		Asset:          asset,
		UpToDate:       sameVersion(current.Version, release.TagName),
	}
	result.ManualCommands = ManualCommands(repo, release.TagName, asset, executable)
	return result, nil
}

type ApplyResult struct {
	Applied bool
	Reason  string
}

func Apply(ctx context.Context, runner subprocess.Runner, repo string, check CheckResult, executable string) (ApplyResult, error) {
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	reason := unsafeReason(check, executable)
	if reason != "" {
		return ApplyResult{Reason: reason}, nil
	}
	tmpDir, err := os.MkdirTemp("", "gitmoot-update-*")
	if err != nil {
		return ApplyResult{}, err
	}
	defer os.RemoveAll(tmpDir)

	target := filepath.Join(tmpDir, "gitmoot")
	_, err = runner.Run(ctx, "", "gh", "release", "download", check.LatestVersion, "--repo", repo, "--pattern", check.Asset.Name, "--output", target)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := verifyDigest(target, check.Asset.Digest); err != nil {
		return ApplyResult{}, err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return ApplyResult{}, err
	}
	if err := replaceExecutable(target, executable); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: true}, nil
}

func ManualCommands(repo string, tag string, asset *Asset, executable string) []string {
	if asset == nil {
		return []string{
			"gh release view " + shellQuote(tag) + " --repo " + shellQuote(repo),
			"gh release download " + shellQuote(tag) + " --repo " + shellQuote(repo) + " --dir /tmp/gitmoot-update",
		}
	}
	downloadPath := "/tmp/gitmoot-update/" + asset.Name
	return []string{
		"mkdir -p /tmp/gitmoot-update",
		"gh release download " + shellQuote(tag) + " --repo " + shellQuote(repo) + " --pattern " + shellQuote(asset.Name) + " --output " + shellQuote(downloadPath),
		"install -m 0755 " + shellQuote(downloadPath) + " " + shellExecutable(executable),
	}
}

func matchingAsset(assets []Asset, goos string, goarch string) *Asset {
	candidates := map[string]struct{}{}
	for _, name := range assetCandidates(goos, goarch) {
		candidates[name] = struct{}{}
	}
	for i := range assets {
		if _, ok := candidates[assets[i].Name]; ok {
			return &assets[i]
		}
	}
	return nil
}

func assetCandidates(goos string, goarch string) []string {
	names := []string{
		"gitmoot_" + goos + "_" + goarch,
		"gitmoot-" + goos + "-" + goarch,
	}
	if goos == "windows" {
		names = append(names, "gitmoot_"+goos+"_"+goarch+".exe", "gitmoot-"+goos+"-"+goarch+".exe")
	}
	return names
}

func sameVersion(current string, latest string) bool {
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")
	latest = strings.TrimPrefix(strings.TrimSpace(latest), "v")
	return current != "" && latest != "" && current == latest
}

func unsafeReason(check CheckResult, executable string) string {
	switch {
	case check.NoRelease:
		return "no GitHub release found"
	case check.UpToDate:
		return "already up to date"
	case check.CurrentVersion == "" || check.CurrentVersion == "dev" || check.CurrentVersion == "unknown":
		return "development builds are not auto-updated"
	case check.Asset == nil:
		return "no exact binary asset matched this platform"
	case !strings.HasPrefix(check.Asset.Digest, "sha256:"):
		return "release asset is missing a sha256 digest"
	case runtime.GOOS == "windows":
		return "in-place replacement is not supported on Windows"
	case strings.TrimSpace(executable) == "":
		return "could not resolve the current executable path"
	}
	if !filepath.IsAbs(executable) {
		return "current executable path is not absolute"
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return "current executable cannot be inspected: " + err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "current executable is a symlink"
	}
	if !info.Mode().IsRegular() {
		return "current executable is not a regular file"
	}
	if err := canWriteDir(filepath.Dir(executable)); err != nil {
		return "install directory is not writable: " + err.Error()
	}
	return ""
}

func canWriteDir(dir string) error {
	file, err := os.CreateTemp(dir, ".gitmoot-update-check-*")
	if err != nil {
		return err
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func verifyDigest(path string, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return errors.New("asset digest must use sha256")
	}
	want := strings.TrimPrefix(digest, "sha256:")
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

func replaceExecutable(source string, target string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".gitmoot-new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	input, err := os.Open(source)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, input); err != nil {
		_ = input.Close()
		_ = tmp.Close()
		return err
	}
	if err := input.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	backup := target + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	return os.Remove(backup)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == ':' || r == '@' || r == '+' || r == '=' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellExecutable(value string) string {
	if strings.TrimSpace(value) == "" {
		return "$(command -v gitmoot)"
	}
	return shellQuote(value)
}

func commandError(result subprocess.Result, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}

func isNotFound(result subprocess.Result) bool {
	text := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(text, "http 404") || strings.Contains(text, "not found")
}
