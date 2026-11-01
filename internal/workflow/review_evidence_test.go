package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #2476. A manifest is accepted only for the reviewed head, within bounds,
// with every file present as a regular file of exactly the pinned size and
// sha256; the pin is what the daemon later re-verifies.
func TestLoadReviewEvidenceRefusesWhatItCannotPin(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	dir := t.TempDir()
	good := filepath.Join(dir, "a.log")
	if err := os.WriteFile(good, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello\n"))
	goodSum := hex.EncodeToString(sum[:])
	write := func(manifestHead string, files ...map[string]any) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"schema_version": 1, "head_sha": manifestHead, "files": files})
		target := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(target, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return target
	}
	entry := func(path string, bytes int, sha string) map[string]any {
		return map[string]any{"path": path, "bytes": bytes, "sha256": sha}
	}

	evidence, err := LoadReviewEvidence(write(head, entry(good, 6, goodSum)), head)
	if err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
	if len(evidence.Files) != 1 || evidence.Files[0].SandboxName != "01-a.log" || evidence.ManifestSHA256 == "" {
		t.Fatalf("pinned evidence = %+v", evidence)
	}
	for name, tc := range map[string]struct {
		manifest string
		want     string
	}{
		"other head":   {write(strings.Repeat("f", 40), entry(good, 6, goodSum)), "not the reviewed head"},
		"wrong size":   {write(head, entry(good, 7, goodSum)), "changed"},
		"wrong hash":   {write(head, entry(good, 6, strings.Repeat("0", 64))), "changed"},
		"symlink":      {write(head, entry(link, 6, goodSum)), "not a regular file"},
		"relative":     {write(head, entry("a.log", 6, goodSum)), "must be absolute"},
		"duplicate":    {write(head, entry(good, 6, goodSum), entry(good, 6, goodSum)), "listed twice"},
		"too large":    {write(head, entry(good, ReviewEvidenceMaxFileBytes+1, goodSum)), "allowed 1-"},
		"missing file": {write(head, entry(filepath.Join(dir, "gone.log"), 6, goodSum)), "no such file"},
	} {
		if _, err := LoadReviewEvidence(tc.manifest, head); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// Drift after pinning is caught by the same check the daemon runs at
	// provisioning.
	if err := os.WriteFile(good, []byte("hello!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Files[0].VerifiedBytes(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("drifted file accepted: %v", err)
	}
}
