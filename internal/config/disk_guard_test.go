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
