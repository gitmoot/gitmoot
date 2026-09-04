package cli

import (
	"os/exec"
	"path"
	"strings"
	"testing"
)

// credentialOverlayFileNames is enumerated from the RESOLVER, not from memory.
// Each name is one this codebase reads as a credential input:
//
//	runtime-auth.env    daemon_runtime_auth.go, runtimeAuthFileName
//	daemon-runtime.env  daemon_runtime_auth.go, legacyRuntimeAuthFileName
//	.credentials.json   daemon_worker.go (claude staging), readonly_seat_credential.go
//	auth.json           daemon_worker.go (codex staging)
//	kimi-code.json      daemon_worker.go (kimi staging)
var credentialOverlayFileNames = []string{
	runtimeAuthFileName,
	legacyRuntimeAuthFileName,
	claudeCredentialsFile,
	"auth.json",
	"kimi-code.json",
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
