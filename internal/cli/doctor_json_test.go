package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestDoctorJSONOutput pins the fix for "gitmoot doctor --json advertised but
// rejected": doctor now accepts --json and emits the checks as a JSON array (each
// with name/status/ok/required/detail) instead of erroring with
// "flag provided but not defined: -json".
func TestDoctorJSONOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	// --repo at a temp dir keeps the run local; individual checks may warn/fail (no
	// git remote, etc.) but the JSON shape is what this test asserts.
	Run([]string{"doctor", "--json", "--repo", t.TempDir()}, &stdout, &stderr)

	if bytes.Contains(stderr.Bytes(), []byte("flag provided but not defined")) {
		t.Fatalf("doctor still rejects --json: %s", stderr.String())
	}
	var checks []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &checks); err != nil {
		t.Fatalf("doctor --json did not produce a JSON array: %v\nstdout=%q", err, stdout.String())
	}
	if len(checks) == 0 {
		t.Fatalf("doctor --json produced an empty array")
	}
	for _, c := range checks {
		for _, k := range []string{"name", "status", "ok", "required", "detail"} {
			if _, ok := c[k]; !ok {
				t.Errorf("doctor --json check missing key %q: %v", k, c)
			}
		}
	}
}
