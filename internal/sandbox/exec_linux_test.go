//go:build linux

package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRuntimeHostReadFilesIncludesOpenSSLConfig(t *testing.T) {
	for _, path := range runtimeHostReadFiles {
		if path == "/etc/ssl/openssl.cnf" {
			return
		}
	}
	t.Fatal("runtime host file grants omit /etc/ssl/openssl.cnf; Node-based review runtimes cannot initialize TLS")
}

// TestReadableRootsGrantsProcfs pins the runtime BOOTSTRAP grant that strict
// read-path mode dropped. Without it the Bun-based Claude/Kimi binaries abort
// and codex's bwrap cannot read /proc/sys/kernel/overflowuid, so every read-only
// review dies before doing any work. Asserted through readableRoots (the
// function execSandbox actually calls) rather than by inspecting a literal list.
func TestReadableRootsGrantsProcfs(t *testing.T) {
	roots, err := readableRoots([]string{t.TempDir()})
	if err != nil {
		t.Fatalf("readableRoots returned error: %v", err)
	}
	for _, root := range roots {
		if root == "/proc" {
			return
		}
	}
	t.Fatalf("readableRoots = %v, want /proc among them; review runtimes cannot bootstrap without procfs", roots)
}

// TestReadableRootsGrantOnlyRequestedAndFixedSystemRoots pins the invariant
// ruling 122157 actually established: NOTHING is promoted into the read set at
// runtime. Every root is either one the caller requested (a daemon-owned staged
// tree) or a member of the fixed, root-owned system list.
//
// AN EARLIER VERSION OF THIS TEST ASSERTED THE WRONG THING. It required
// /usr/local to be absent, on the reasoning that operators install there. CI
// refuted it: GitHub runners install Go at /usr/local/go, so a seat hit
// "go: Permission denied", exit 126 - reintroducing the #1918 symptom this work
// removes. The ruling deleted the PROMOTION of a root chosen at runtime from
// PATH, not the fixed system set, and all three measured exposures were profile
// or install roots. Encoding a claim the ruling never made cost a real
// capability, so the assertion now names the boundary instead of a prefix.
func TestReadableRootsGrantOnlyRequestedAndFixedSystemRoots(t *testing.T) {
	requested := t.TempDir()
	roots, err := readableRoots([]string{requested})
	if err != nil {
		t.Fatal(err)
	}
	fixed := map[string]bool{
		"/bin": true, "/sbin": true, "/lib": true, "/lib64": true, "/dev": true,
		"/usr/bin": true, "/usr/sbin": true, "/usr/lib": true, "/usr/lib64": true,
		"/usr/libexec": true, "/usr/share": true,
		"/usr/local/bin": true, "/usr/local/sbin": true, "/usr/local/lib": true,
		"/usr/local/lib64": true, "/usr/local/share": true,
		"/etc/ssl/certs": true, "/etc/pki": true, "/proc": true,
		"/sys/devices/system/cpu": true, "/sys/bus/pci": true, "/etc/fonts": true,
	}
	pciTrees, _ := filepath.Glob("/sys/devices/pci*")
	for _, tree := range pciTrees {
		fixed[tree] = true
	}
	for _, root := range roots {
		if root == requested || fixed[root] {
			continue
		}
		t.Errorf("readableRoots granted %q, which is neither the requested staged root nor a fixed system root: a runtime-chosen root has been promoted back into the read set", root)
	}
	// The requested root must survive, or a passing test could mean the read set
	// dropped the staged tree the seat actually needs.
	if !slices.Contains(roots, requested) {
		t.Fatalf("readableRoots = %v, missing the requested root %q", roots, requested)
	}
	// A HOME-shaped root must never appear unrequested: that is the profile
	// exposure class (kimi's ~/.kimi-code, claude's ~/.local/share/claude).
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, root := range roots {
			if root == home || strings.HasPrefix(root, home+string(os.PathSeparator)) {
				t.Errorf("readableRoots granted %q inside the operator home %q", root, home)
			}
		}
	}
}

// #2505 (lenscale-admin#37/#38): a read-only review seat names its reads, so it
// sees only the fixed system list. Headless Chromium then could not start (it
// reads the CPU topology) or find fonts, and every UI review needing a rendered
// check blocked. Run through the real launcher exactly as a review seat is
// launched (--read-only-workdir plus explicit reads, no /etc or /sys grant),
// the confined process must read both, and still not read an unrelated /etc
// file.
func TestReadOnlySeatCanReadBrowserSystemFilesE2E(t *testing.T) {
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	requireReadOnlySeatLaunchable(t, gitmoot)
	probeDir := t.TempDir()
	probe := filepath.Join(probeDir, "noexecprobe")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", probe, "./internal/sandbox/testdata/noexecprobe")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, output)
	}
	workdir, write := t.TempDir(), t.TempDir()
	read := func(path string) (string, error) {
		cmd := exec.Command(gitmoot, "sandbox-exec", "--read-only-workdir", "--read", probeDir, "--write", write, "--", probe, "read", path)
		cmd.Dir = workdir
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	for _, path := range []string{"/sys/devices/system/cpu/online", "/etc/fonts/fonts.conf"} {
		if _, err := os.Stat(path); err != nil {
			t.Logf("skip %s: absent on this host", path)
			continue
		}
		if out, err := read(path); err != nil || !strings.HasPrefix(out, "OK read") {
			t.Fatalf("review seat cannot read %s: %v %q", path, err, out)
		}
	}
	// The grant stays narrow: other /etc files remain unreadable.
	if _, err := os.Stat("/etc/hostname"); err == nil {
		if out, err := read("/etc/hostname"); err == nil || !strings.Contains(out, "DENIED") {
			t.Fatalf("review seat read /etc/hostname: %v %q", err, out)
		}
	}
}

// #2505: the outcome that matters, measured with a real headless Chromium when
// the host has one (GITMOOT_TEST_CHROME or chrome-headless-shell/chromium on
// PATH). Launched exactly as in a review seat, it must render a page; before
// the fix it exited 133 ("GPU process isn't usable").
func TestReadOnlySeatRunsHeadlessChromiumE2E(t *testing.T) {
	chrome := os.Getenv("GITMOOT_TEST_CHROME")
	for _, name := range []string{"chrome-headless-shell", "chromium", "chromium-browser"} {
		if chrome != "" {
			break
		}
		chrome, _ = exec.LookPath(name)
	}
	if chrome == "" {
		t.Skip("no headless Chromium on this host; set GITMOOT_TEST_CHROME")
	}
	requireLandlockABI(t)
	gitmoot := buildGitmootBinary(t)
	requireReadOnlySeatLaunchable(t, gitmoot)
	workdir, write := t.TempDir(), t.TempDir()
	cmd := exec.Command(gitmoot, "sandbox-exec", "--read-only-workdir", "--read", filepath.Dir(chrome), "--write", write, "--",
		chrome, "--no-sandbox", "--disable-dev-shm-usage", "--user-data-dir="+filepath.Join(write, "profile"),
		"--dump-dom", "data:text/html,<p>rendered</p>")
	cmd.Dir = workdir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil || !strings.Contains(out.String(), "<p>rendered</p>") {
		t.Fatalf("headless Chromium did not render in a review seat: %v\n%s", err, out.String())
	}
}
