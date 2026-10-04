package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// THE MODULE CACHE A READ-ONLY SEAT BUILDS FROM (#2314).
//
// Root cause of the blocked offline reviews: every seat's GOMODCACHE was
// <tool cache>/read-only/<hash of the job's checkout path>/go-mod. Each
// review gets its own checkout path, so that directory was new and EMPTY for
// every review, and nothing ever filled it. Reviews run offline, so
// `GOPROXY=off go build` failed before compiling anything. On 2026-10-04 the
// dispatcher pre-filled a cache at read-only/41839e54599e81d9/go-mod and
// named it in the review instructions, but that hash belongs to a different
// checkout path, so the Landlock grant (correctly) did not cover it, and the
// seat's own read-only/<hash>/go-mod was empty. There was no way to hand a
// seat a populated cache.
//
// The fix: before the seat starts, the daemon runs `go mod download` for the
// checkout's modules into a cache it owns, one per repository, and grants the
// seat that cache READ-ONLY. Repeated reviews of the same repository reuse it,
// so after the first review the download is a no-op.
//
// The download is itself confined. It runs through the same Landlock
// sandbox-exec as the seat, with the checkout read-only and only the module
// cache and a scratch dir writable. `go mod download` does not run repository
// code. Its environment is built from scratch: the network settings are the
// daemon's own (GOPROXY, GOSUMDB, GONOSUMDB, GOPRIVATE, GONOPROXY), except
// that `direct` and `off` are dropped from GOPROXY and GOVCS=*:off, so no
// version-control tool is ever run against a URL the checkout chose.
// GOTOOLCHAIN=local stops a toolchain download and GOENV=off ignores any go
// env file.
//
// No seat ever gets write access to this cache, so one review cannot plant a
// module for the next. The cache is kept apart from the shared tool cache,
// which ordinary isolated (non-seat) jobs can write.

const (
	seatGoModPrefetchTimeout = 5 * time.Minute
	seatGoModMaxDepth        = 4
	seatGoModMaxModules      = 16
	seatGoModMaxEntries      = 50000
	seatGoModDefaultProxy    = "https://proxy.golang.org"
)

// seatGoModCacheDir is the daemon-owned module cache for one repository.
func seatGoModCacheDir(home, repo string) string {
	key := strings.ToLower(strings.TrimSpace(repo))
	if key == "" {
		key = "local"
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(home, "cache", "seat-go-mod", fmt.Sprintf("%x", sum[:8]))
}

type seatGoModuleRequest struct {
	paths      config.Paths
	checkout   string
	reviewRepo string
	// goroot is the STAGED toolchain root the seat will run.
	goroot string
	// gowork is the GOWORK value staging decided on: "off" or an absolute file.
	gowork string
	// scratch is a job-private directory the download may write and that is
	// removed afterwards.
	scratch string
	// writes are the seat's write grants; the cache must not overlap them.
	writes []string
}

// stageSeatGoModules fills the repository's module cache and returns it, or ""
// when the checkout has no Go module or the cache cannot be used safely. The
// diagnostic names what went wrong; a partial download still returns the
// cache, because a review that cannot reach the network is better served by
// most modules than by none.
func stageSeatGoModules(request seatGoModuleRequest) (string, string) {
	roots, workspace := seatGoModuleRoots(request.checkout, request.gowork)
	if len(roots) == 0 {
		return "", ""
	}
	cache := seatGoModCacheDir(request.paths.Home, request.reviewRepo)
	policy, err := config.LoadToolCache(request.paths)
	if err != nil {
		return "", fmt.Sprintf("load tool cache config: %v; seat keeps an empty private module cache", err)
	}
	if dir := strings.TrimSpace(policy.Dir); dir != "" && pathsOverlap(filepath.Clean(dir), cache) {
		return "", fmt.Sprintf("module cache %q overlaps the shared tool cache %q, which non-seat jobs can write; seat keeps an empty private module cache", cache, dir)
	}
	if err := validateStagedToolchainPlacement(cache, request.writes); err != nil {
		return "", fmt.Sprintf("%v; seat keeps an empty private module cache", err)
	}
	proxy := seatGoProxy(os.Getenv("GOPROXY"))
	if proxy == "" {
		return "", fmt.Sprintf("GOPROXY %q names no module proxy (direct and off are not used for seats); seat keeps an empty private module cache", os.Getenv("GOPROXY"))
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return "", fmt.Sprintf("create module cache %q: %v; seat keeps an empty private module cache", cache, err)
	}
	if err := os.MkdirAll(request.scratch, 0o700); err != nil {
		return "", fmt.Sprintf("create module download scratch %q: %v; seat keeps an empty private module cache", request.scratch, err)
	}
	defer func() { _ = removeTreeForcibly(request.scratch) }()

	gowork := "off"
	var readFiles []string
	if workspace {
		gowork = request.gowork
		readFiles = append(readFiles, request.gowork)
	}
	runner := subprocess.WrappingRunner{
		Inner:           subprocess.CuratedGroupRunner{BaseEnv: seatGoModuleEnv(request.goroot, cache, proxy, gowork, request.scratch)},
		ReadablePaths:   append([]string{request.checkout, request.goroot}, seatGoFileProxyDirs(proxy)...),
		ReadableFiles:   readFiles,
		WritablePaths:   []string{cache, request.scratch},
		ReadOnlyWorkdir: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), seatGoModPrefetchTimeout)
	defer cancel()
	goBinary := filepath.Join(request.goroot, "bin", "go")
	var failures []string
	for _, root := range roots {
		result, err := runner.Run(ctx, root, goBinary, "mod", "download")
		if err != nil {
			relative, relErr := filepath.Rel(request.checkout, root)
			if relErr != nil {
				relative = root
			}
			failures = append(failures, fmt.Sprintf("go mod download in %s: %v: %s", relative, err, lastLines(result.Stderr, 3)))
		}
	}
	if len(failures) > 0 {
		return cache, strings.Join(failures, "; ")
	}
	return cache, ""
}

// seatGoModuleEnv is the whole environment of the download. Only the daemon's
// own Go network settings and HTTP proxy and certificate settings are carried
// over; see the comment at the top of this file.
func seatGoModuleEnv(goroot, cache, proxy, gowork, scratch string) []string {
	env := []string{
		"PATH=" + filepath.Join(goroot, "bin") + ":/usr/bin:/bin",
		"GOROOT=" + goroot,
		"GOTOOLCHAIN=local",
		"GOENV=off",
		// -modcacherw keeps the cache removable by the daemon. The seat's
		// access is decided by Landlock, which grants it read only.
		"GOFLAGS=-mod=readonly -modcacherw",
		"GOMODCACHE=" + cache,
		"GOPROXY=" + proxy,
		"GOVCS=*:off",
		"GOWORK=" + gowork,
		"CGO_ENABLED=0",
		"HOME=" + scratch,
		"TMPDIR=" + scratch,
		"GOCACHE=" + filepath.Join(scratch, "go-build"),
		"GOPATH=" + filepath.Join(scratch, "go"),
	}
	for _, name := range []string{
		"GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GONOPROXY",
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// seatGoFileProxyDirs returns the local directories named by file:// proxy
// entries. The download needs to read them; the seat never does.
func seatGoFileProxyDirs(proxy string) []string {
	var dirs []string
	for _, entry := range strings.Split(proxy, ",") {
		dir, ok := strings.CutPrefix(entry, "file://")
		if !ok || !filepath.IsAbs(dir) {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, filepath.Clean(dir))
		}
	}
	return dirs
}

// seatGoProxy keeps only real proxy URLs from a GOPROXY list. "direct" would
// make the go command run version-control tools against URLs the checkout
// chose; "off" ends the list. An unset GOPROXY means the public proxy.
func seatGoProxy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return seatGoModDefaultProxy
	}
	var kept []string
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '|' }) {
		entry = strings.TrimSpace(entry)
		if entry == "off" {
			break
		}
		if entry == "" || entry == "direct" {
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, ",")
}

// seatGoModuleRoots finds where to run `go mod download`. With a workspace
// file that is the checkout root, once. Otherwise it is every directory below
// the checkout holding a go.mod, found by a bounded walk that does not follow
// symlinks and skips vendor, testdata, node_modules and hidden directories.
func seatGoModuleRoots(checkout, gowork string) ([]string, bool) {
	checkout = filepath.Clean(checkout)
	var roots []string
	visited := 0
	_ = filepath.WalkDir(checkout, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() && path != checkout {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > seatGoModMaxEntries {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if path == checkout {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			relative, relErr := filepath.Rel(checkout, path)
			if relErr != nil || strings.Count(relative, string(filepath.Separator))+1 > seatGoModMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "go.mod" && entry.Type().IsRegular() {
			roots = append(roots, filepath.Dir(path))
			if len(roots) >= seatGoModMaxModules {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(roots) == 0 {
		return nil, false
	}
	gowork = strings.TrimSpace(gowork)
	if gowork != "" && gowork != "off" && filepath.IsAbs(gowork) {
		return []string{checkout}, true
	}
	return roots, false
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}

// withSeatGoModuleCache points the seat at the populated cache. Entries for
// GOMODCACHE and GOPROXY are replaced rather than appended so the seat env has
// one value for each. GOPROXY=off makes a missing module fail as "module lookup
// disabled" instead of as a permission error on the read-only cache.
func withSeatGoModuleCache(env []string, cache string) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name == "GOMODCACHE" || name == "GOPROXY" {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GOMODCACHE="+cache, "GOPROXY=off")
}
