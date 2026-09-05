package sandbox

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSandboxExecResolvesInheritedPathBinaryWithoutGrantingProfileKernelE2E is
// #1918 and its #1921 review finding measured together, at the boundary that
// decides both.
//
// It runs the real hidden sandbox-exec shim, so argv[0] goes through the same
// execLookPath the launch failure came from — the distinction the review raised
// against the earlier E2E, which asked a SHELL seat to `command -v` and
// therefore only proved shell lookup: a shell seat's own argv[0] is /bin/sh,
// which is found whatever PATH says.
//
// The fixture reproduces the host layout that made this a security finding
// rather than a convenience one: kimi is a self-contained executable alone in
// ~/.kimi-code/bin, and ~/.kimi-code also holds credentials/ and oauth/. Both
// properties are asserted from INSIDE the sandbox in a single run:
//
//   - the binary must be RESOLVABLE through the inherited PATH (#1918), and
//   - its profile root must NOT be READABLE (#1921 review P1).
//
// Before the promotion fix the second assertion fails while the first passes,
// which is exactly the state PR #1921 shipped: availability restored, and a
// credential grant restored with it.
func TestSandboxExecResolvesInheritedPathBinaryWithoutGrantingProfileKernelE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)

	// NOT t.TempDir(): writableRoots grants os.TempDir() and /tmp IMPLICITLY, so
	// a fixture under /tmp is readable whatever the promotion rule decides and
	// this test would pass for the wrong reason (measured — it reported
	// CREDENTIAL=READABLE with the fix in place). The sibling kernel E2Es use
	// MkdirTemp(".") for the same reason.
	base, err := os.MkdirTemp(".", ".gitmoot-profile-grant-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(base, ".kimi-code")
	profileBin := filepath.Join(profile, "bin")
	credentials := filepath.Join(profile, "credentials")
	workdir := filepath.Join(base, "seat-worktree")
	for _, dir := range []string{profileBin, credentials, workdir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	secret := filepath.Join(credentials, "kimi-code.json")
	if err := os.WriteFile(secret, []byte(`{"access_token":"seat-must-not-read-this"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The probe IS the resolved runtime binary, so it reports on its own
	// environment. It prints a verdict for each half rather than exiting
	// non-zero, so a failure names WHICH half broke: an exit code alone cannot
	// distinguish "not resolvable" from "read the credential".
	probe := filepath.Join(profileBin, "kimi")
	script := "#!/bin/sh\n" +
		"if cat \"$1\" >/dev/null 2>&1; then printf 'CREDENTIAL=READABLE\\n'; else printf 'CREDENTIAL=DENIED\\n'; fi\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// argv[0] is the BARE NAME on purpose: resolving it is the mechanism under
	// test. PATH carries the profile bin dir the way seatPath's inherited PATH
	// does on the host.
	cmd := exec.Command(gitmoot, "sandbox-exec", "--read", workdir, "--", "kimi", secret)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "PATH="+profileBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the seat could not resolve and launch a runtime binary that is only on the inherited PATH.\nThis is the #1918 failure at the true boundary: sandbox-exec resolves argv[0] with exec.LookPath before any Landlock rule is applied.\nerr=%v\noutput=%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "CREDENTIAL=DENIED" {
		t.Fatalf("seat verdict = %q, want %q.\nPromoting the resolved binary's install root made the runtime's credential store readable: %s holds credentials/ and oauth/, and granting it hands every read-only seat the operator's account.", got, "CREDENTIAL=DENIED", profile)
	}
}

// TestExecutableInstallRootPromotionSkipsCredentialProfiles pins the
// discriminator itself, because the E2E above can only prove the case its
// fixture builds.
//
// The two directions matter equally and pull against each other: a node-packaged
// runtime is UNRUNNABLE without its package root (codex resolves to
// <node>/lib/node_modules/@openai/codex/bin/codex.js), so a fix that simply
// stopped promoting would trade a credential leak for a launch failure. Only the
// profile case may lose the grant.
func TestExecutableInstallRootPromotionRequiresProvenPackageTree(t *testing.T) {
	base := t.TempDir()

	packageRoot := filepath.Join(base, "node_modules", "@openai", "codex")
	packageBin := filepath.Join(packageRoot, "bin")
	// A profile holding NO recognized credential name at all: this is review F1's
	// fixture. The old rule promoted exactly this shape, and a kernel probe then
	// read the api_key out of config.toml.
	profile := filepath.Join(base, ".kimi-code")
	profileBin := filepath.Join(profile, "bin")
	for _, dir := range []string{packageBin, profileBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "config.toml"), []byte("api_key = \"seat-must-not-read-this\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{filepath.Join(packageBin, "codex.js"), filepath.Join(profileBin, "kimi")} {
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(executable string) map[string]bool {
		t.Helper()
		granted := map[string]bool{}
		add := func(candidate string, _ bool) error {
			granted[filepath.Clean(candidate)] = true
			return nil
		}
		if err := addExecutableReadRoots(add, executable); err != nil {
			t.Fatalf("addExecutableReadRoots(%q): %v", executable, err)
		}
		return granted
	}

	// Availability direction. A node-packaged runtime is UNRUNNABLE without its
	// package root, so this half is what stops the fix from trading a credential
	// leak for a launch failure.
	packageGrants := collect(filepath.Join(packageBin, "codex.js"))
	if !packageGrants[packageBin] {
		t.Errorf("the exec dir %q was not granted, so the runtime cannot launch at all", packageBin)
	}
	if !packageGrants[packageRoot] {
		t.Errorf("a proven package tree lost its root %q; codex cannot run without the files beside its bin dir", packageRoot)
	}

	// Containment direction, and note the profile carries NO name from the old
	// denylist. Under the previous rule this passed while leaking.
	profileGrants := collect(filepath.Join(profileBin, "kimi"))
	if !profileGrants[profileBin] {
		t.Errorf("the exec dir %q was not granted, so the runtime cannot launch at all", profileBin)
	}
	if profileGrants[profile] {
		t.Errorf("profile %q was promoted with no proof it is a package tree; its config.toml carries api_key and OAuth material (review F1)", profile)
	}
}

// TestIsPackageInstallRootRequiresBothHalvesAndFailsClosed pins the
// discriminator's edges, each of which is a way a planted marker could buy a
// grant it should not get.
func TestIsPackageInstallRootRequiresBothHalvesAndFailsClosed(t *testing.T) {
	base := t.TempDir()

	proven := filepath.Join(base, "node_modules", "pkg")
	strayManifest := filepath.Join(base, ".profile-root")
	nodeModulesNoManifest := filepath.Join(base, "node_modules", "bare")
	lookalike := filepath.Join(base, "node_modules_backup", "pkg")
	manifestIsDir := filepath.Join(base, "node_modules", "dirmanifest")
	for _, dir := range []string{proven, strayManifest, nodeModulesNoManifest, lookalike, filepath.Join(manifestIsDir, "package.json")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{proven, strayManifest, lookalike} {
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if !isPackageInstallRoot(proven) {
		t.Errorf("a real package root %q was rejected; every node-packaged runtime would stop launching", proven)
	}
	if isPackageInstallRoot(strayManifest) {
		t.Errorf("profile %q bought a grant with a stray package.json and no node_modules ancestor", strayManifest)
	}
	if isPackageInstallRoot(nodeModulesNoManifest) {
		t.Errorf("directory %q under node_modules with no manifest was accepted as a package root", nodeModulesNoManifest)
	}
	if isPackageInstallRoot(lookalike) {
		t.Errorf("path %q matched node_modules as a substring rather than a whole path element", lookalike)
	}
	if isPackageInstallRoot(manifestIsDir) {
		t.Errorf("a DIRECTORY named package.json in %q was accepted; only a regular manifest may prove a package tree", manifestIsDir)
	}
	if isPackageInstallRoot(filepath.Join(base, "node_modules", "absent")) {
		t.Errorf("a nonexistent root was accepted; an Lstat error must fail closed")
	}
}

// TestSandboxExecDeniesCredentialCreatedAfterSetupKernelE2E is review F2 at the
// only boundary that can show it: the seat is already RUNNING when the
// credential appears.
//
// The old rule made a time-of-check decision (Lstat the direct children) and
// then installed a RECURSIVE Landlock grant, so a credential created afterwards
// landed inside an already-granted root. The reviewer proved it with a
// synchronized probe that waited for the seat to enter, created
// profile/credentials/late-token.json externally, and read it back:
// LATE_CREDENTIAL=READABLE.
//
// The synchronisation is the whole test, so it is explicit rather than timed:
// the seat announces it is inside by creating a ready file in its writable
// workdir, then blocks until the test creates a go file. Only then does it read.
// A sleep would make this flaky in both directions; a file handshake makes the
// ordering an invariant.
//
// Under the inverted rule this cannot fail for a new reason: the profile root is
// never granted at all, so there is no window in which a later descendant
// becomes readable.
func TestSandboxExecDeniesCredentialCreatedAfterSetupKernelE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)

	// Not t.TempDir(): /tmp is granted implicitly, which would make this pass for
	// the wrong reason. Same reasoning as the sibling E2E above.
	base, err := os.MkdirTemp(".", ".gitmoot-late-cred-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}

	profile := filepath.Join(base, ".kimi-code")
	profileBin := filepath.Join(profile, "bin")
	credentials := filepath.Join(profile, "credentials")
	workdir := filepath.Join(base, "seat-worktree")
	for _, dir := range []string{profileBin, workdir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	late := filepath.Join(credentials, "late-token.json")
	ready := filepath.Join(workdir, "seat-entered")
	proceed := filepath.Join(workdir, "credential-planted")

	probe := filepath.Join(profileBin, "kimi")
	script := "#!/bin/sh\n" +
		": > \"$2\"\n" +
		"i=0\n" +
		"while [ ! -e \"$3\" ] && [ $i -lt 200 ]; do i=$((i+1)); sleep 0.05; done\n" +
		"if [ ! -e \"$3\" ]; then printf 'HANDSHAKE=TIMEOUT\\n'; exit 0; fi\n" +
		"if cat \"$1\" >/dev/null 2>&1; then printf 'LATE_CREDENTIAL=READABLE\\n'; else printf 'LATE_CREDENTIAL=DENIED\\n'; fi\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(gitmoot, "sandbox-exec", "--read", workdir, "--write", workdir, "--", "kimi", late, ready, proceed)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "PATH="+profileBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for the seat to be INSIDE the sandbox before the credential exists.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, statErr := os.Stat(ready); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the sandboxed seat never signalled that it had entered; the handshake, not the grant, is broken")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The credential is created AFTER the sandbox is established. This is the
	// exact sequence the reviewer used.
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(late, []byte(`{"access_token":"created-after-the-scan"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proceed, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("sandboxed seat failed: %v\noutput=%s", err, output)
	}
	got := strings.TrimSpace(string(output))
	if got == "HANDSHAKE=TIMEOUT" {
		t.Fatal("the seat timed out waiting for the planted credential, so this run measured nothing")
	}
	if got != "LATE_CREDENTIAL=DENIED" {
		t.Fatalf("seat verdict = %q, want %q.\nA credential CREATED AFTER sandbox setup was readable, so promotion of a mutable operator root is a time-of-check decision guarding a recursive grant (review F2). Withhold the root instead of scanning it.", got, "LATE_CREDENTIAL=DENIED")
	}
}
