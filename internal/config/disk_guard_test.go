package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDiskGuardPolicyDefaultsEnabled(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	got, err := LoadDiskGuardPolicy(paths)
	if err != nil {
		t.Fatalf("LoadDiskGuardPolicy: %v", err)
	}
	if got != DefaultDiskGuardPolicy() {
		t.Fatalf("policy = %+v, want %+v", got, DefaultDiskGuardPolicy())
	}
	if !got.Enabled {
		t.Fatal("default disk guard must be enabled")
	}
}

func TestLoadDiskGuardPolicyParsesSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[disk_guard]
enabled = true
min_free_bytes = 123456
min_free_percent = 12.5
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDiskGuardPolicy(paths)
	if err != nil {
		t.Fatalf("LoadDiskGuardPolicy: %v", err)
	}
	if !got.Enabled || got.MinFreeBytes != 123456 || got.MinFreePercent != 12.5 {
		t.Fatalf("policy = %+v", got)
	}
}

func TestLoadDiskGuardPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "bad boolean", body: "enabled = maybe", want: "enabled"},
		{name: "negative bytes", body: "min_free_bytes = -1", want: "min_free_bytes"},
		{name: "percent too high", body: "min_free_percent = 101", want: "between 0 and 100"},
		{name: "no enabled floor", body: "min_free_bytes = 0\nmin_free_percent = 0", want: "requires"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := os.MkdirAll(paths.Home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte("[disk_guard]\n"+tc.body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDiskGuardPolicy(paths)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestLoadDiskGuardPolicyMalformedHeaderEndsTheSection pins the malformed-header
// reset that disk_guard.go shares with tool_cache.go: a header with no closing
// bracket ENDS whatever section was active rather than silently continuing it.
//
// Without the reset, the "[disk_guard" typo below would leave [disk_guard] the
// active section, and min_free_percent = 99 would be applied to the policy - the
// same class of bug the #1113 finder raised for [cache].
//
// This test exists because the behaviour was UNPINNED. Measured by deleting the
// reset branch from disk_guard.go: the mutant compiled and the whole
// internal/config package stayed green, while the equivalent mutant in
// tool_cache.go is caught by TestLoadToolCache. A shared section scanner would
// therefore have been free to drop the reset here and nothing would have said so.
func TestLoadDiskGuardPolicyMalformedHeaderEndsTheSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[disk_guard]
enabled = true
min_free_bytes = 111
[disk_guard
min_free_percent = 99
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDiskGuardPolicy(paths)
	if err != nil {
		t.Fatalf("LoadDiskGuardPolicy: %v", err)
	}
	if got.MinFreeBytes != 111 {
		t.Fatalf("MinFreeBytes = %d, want 111 (keys before the malformed header must still apply)", got.MinFreeBytes)
	}
	if got.MinFreePercent == 99 {
		t.Fatal("min_free_percent after a malformed header was applied to [disk_guard]; the header must end the section")
	}
	if got.MinFreePercent != DefaultDiskGuardPolicy().MinFreePercent {
		t.Fatalf("MinFreePercent = %v, want the default %v", got.MinFreePercent, DefaultDiskGuardPolicy().MinFreePercent)
	}
}
