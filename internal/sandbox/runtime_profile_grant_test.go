package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
func TestExecutableInstallRootPromotionSkipsCredentialProfiles(t *testing.T) {
	base := t.TempDir()

	packageRoot := filepath.Join(base, "node_modules", "@openai", "codex")
	packageBin := filepath.Join(packageRoot, "bin")
	profile := filepath.Join(base, ".kimi-code")
	profileBin := filepath.Join(profile, "bin")
	for _, dir := range []string{packageBin, profileBin, filepath.Join(profile, "oauth")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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

	packageGrants := collect(filepath.Join(packageBin, "codex.js"))
	if !packageGrants[packageBin] {
		t.Errorf("the exec dir %q was not granted, so the runtime cannot launch at all", packageBin)
	}
	if !packageGrants[packageRoot] {
		t.Errorf("an ordinary installation tree lost its package root %q; codex cannot run without the files beside its bin dir", packageRoot)
	}

	profileGrants := collect(filepath.Join(profileBin, "kimi"))
	if !profileGrants[profileBin] {
		t.Errorf("the exec dir %q was not granted, so the runtime cannot launch at all", profileBin)
	}
	if profileGrants[profile] {
		t.Errorf("credential-bearing profile %q was promoted to a readable root; oauth/ and credentials/ live there", profile)
	}
}

// TestHoldsCredentialMaterialFailsClosed covers the branch the two tests above
// cannot reach: a candidate root that cannot be inspected.
//
// Withholding a grant degrades to a launch failure that names itself, whereas
// granting one leaks an account, so the unreadable case must read as
// credential-bearing. A permission-denied Lstat is the realistic shape.
func TestHoldsCredentialMaterialFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits, so an unstattable candidate cannot be staged")
	}
	base := t.TempDir()
	unreadable := filepath.Join(base, "opaque")
	if err := os.MkdirAll(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })

	if !holdsCredentialMaterial(unreadable) {
		t.Fatalf("an uninspectable candidate %q was treated as safe to promote; this path must fail closed", unreadable)
	}
	if holdsCredentialMaterial(base) {
		t.Fatalf("an ordinary readable directory %q was reported as credential-bearing, which would strip grants every runtime needs", base)
	}
}
