package cli

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// credentialOverlayFileNames is enumerated from the RESOLVER, not from memory.
// Each name is one this codebase reads as a credential input:
//
//	runtime-auth.env    daemon_runtime_auth.go, runtimeAuthFileName
//	daemon-runtime.env  daemon_runtime_auth.go, legacyRuntimeAuthFileName
//	.credentials.json   daemon_worker.go (claude staging), readonly_seat_credential.go
//	auth.json           daemon_worker.go (codex staging)
//	kimi-code.json      daemon_worker.go (kimi staging)
//	keychain.env        pipeline/env_runtime.go, ResolveKeychainPath
//	bridge.token        bridge.go, bridgeTokenName
//
// The first five were the list this guard shipped with, and it was INCOMPLETE:
// the review measured keychain.env and bridge.token resolving through the same
// class of code with neither an ignore rule nor a guard entry. The enumeration
// is now derived by searching every credential-adjacent filename literal in
// internal/ and cmd/, not by recalling the ones already known.
var credentialOverlayFileNames = []string{
	runtimeAuthFileName,
	legacyRuntimeAuthFileName,
	claudeCredentialsFile,
	"auth.json",
	"kimi-code.json",
	"keychain.env",
	bridgeTokenName,
}

// This guard enters through the REAL REPOSITORY INDEX, because the index is
// what failed: #1810 tracked internal/cli/runtime-auth.env holding a live
// Anthropic OAuth token and pushed it to a public remote. A test that inspected
// a temp directory, or that scanned file CONTENT, would not have caught it -
// content scanning also fires on legitimate planted fixtures elsewhere in this
// repo (#1807 carries nine), so it gets switched off. Scope by FILENAME and ask
// git what is actually tracked.
//
// MUTANT: revert the .gitignore rules and `git add -f` one of these names, and
// this test fails - it reads `git ls-files`, so the file only has to reach the
// index, not a commit.
//
// Note this is currently the ONLY automated defence: GitHub secret scanning is
// disabled on this repository.
func TestNoCredentialOverlayFileIsTracked(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout, nothing to guard: %v", err)
	}
	repo := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", repo, err)
	}
	tracked := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(tracked) < 2 {
		t.Fatalf("git ls-files returned %d paths in %s; the guard measured nothing", len(tracked), repo)
	}
	banned := make(map[string]struct{}, len(credentialOverlayFileNames))
	for _, name := range credentialOverlayFileNames {
		banned[name] = struct{}{}
	}
	var found []string
	for _, file := range tracked {
		if file == "" {
			continue
		}
		if _, bad := banned[path.Base(file)]; bad {
			found = append(found, file)
		}
	}
	if len(found) > 0 {
		t.Fatalf("credential-bearing overlay files are TRACKED: %v. This repository is public and secret scanning is disabled; remove them, rotate the credential, and keep the .gitignore rules that stop them returning", found)
	}
	t.Logf("scanned %d tracked paths against %d credential overlay names, 0 matches", len(tracked), len(credentialOverlayFileNames))
}

// The ignore rules and the tracked-file guard stop a credential reaching the
// INDEX. This asserts the property one level earlier, at the WRITERS: a
// credential must never be placed relative to the working directory in the
// first place. bridge.token had no such guard when #1810 was reviewed.
func TestCredentialWritersRefuseANonAbsoluteHome(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{"", "   ", "relative/home", "."} {
		if err := requireAbsoluteCredentialHome(home, "bridge.token"); err == nil {
			t.Fatalf("requireAbsoluteCredentialHome(%q) accepted a non-absolute home", home)
		}
		if _, err := ensureBridgeToken(config.Paths{Home: home}, false); err == nil {
			t.Fatalf("ensureBridgeToken accepted home %q; a token would be written relative to the working directory", home)
		}
	}
	// Nothing may have been created beside the package source while proving it.
	for _, name := range credentialOverlayFileNames {
		if _, err := os.Stat(filepath.Join(cwd, name)); err == nil {
			_ = os.Remove(filepath.Join(cwd, name))
			t.Fatalf("a credential file %q was created in the work tree while asserting the guard", name)
		}
	}
	// An absolute home still works: the guard must not break the feature.
	paths := config.Paths{Home: t.TempDir()}
	tokenPath, err := ensureBridgeToken(paths, false)
	if err != nil {
		t.Fatalf("ensureBridgeToken with an absolute home: %v", err)
	}
	if !filepath.IsAbs(tokenPath) || filepath.Dir(tokenPath) != paths.Home {
		t.Fatalf("token path %q is not inside the absolute home %q", tokenPath, paths.Home)
	}
}

// THE THIRD WRITER. prepareReadOnlyRuntimeState stages three of the seven
// overlay names by joining onto cacheRoot, and it was the one writer the
// previous head left unguarded: driven with a relative cacheRoot it returned a
// relative stateDir and wrote auth.json there. The cacheRoot must EXIST, or the
// test would pass on a later mkdir failure rather than on the guard.
func TestPrepareReadOnlyRuntimeStateRefusesARelativeCacheRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	const relative = "relative-cache"
	if err := os.MkdirAll(relative, 0o700); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"tokens":{"access":"placeholder-not-a-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Name: "reviewer", Runtime: runtime.CodexRuntime, ReadOnlySeat: true, RuntimeConfigDir: source}

	stateDir, env, _, err := prepareReadOnlyRuntimeState(agent, relative, false)
	if err == nil {
		t.Fatalf("a relative cacheRoot was accepted: stateDir=%q env=%v; credentials would be staged inside the working directory", stateDir, env)
	}
	if stateDir != "" {
		t.Fatalf("stateDir = %q on refusal, want empty", stateDir)
	}
	// Nothing may have been created under the relative root.
	var created []string
	_ = filepath.Walk(relative, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info != nil && !info.IsDir() {
			created = append(created, path)
		}
		return nil
	})
	if len(created) > 0 {
		t.Fatalf("the refused call still wrote %v under the relative cacheRoot", created)
	}

	// An ABSOLUTE cacheRoot must still stage and inject: the guard is a
	// precondition, not a feature removal.
	absolute := t.TempDir()
	stateDir, env, _, err = prepareReadOnlyRuntimeState(agent, absolute, false)
	if err != nil {
		t.Fatalf("absolute cacheRoot rejected: %v", err)
	}
	if !filepath.IsAbs(stateDir) || !strings.HasPrefix(stateDir, absolute) {
		t.Fatalf("stateDir = %q, want an absolute path under %q", stateDir, absolute)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "auth.json")); statErr != nil {
		t.Fatalf("absolute cacheRoot did not stage the credential: %v", statErr)
	}
	var injected bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_HOME=") {
			injected = true
		}
	}
	if !injected {
		t.Fatalf("absolute cacheRoot did not inject the runtime state env: %v", env)
	}
}
