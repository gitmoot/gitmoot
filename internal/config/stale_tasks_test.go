package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadStaleTaskTTL(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    time.Duration
		wantErr bool
	}{
		{name: "missing", want: DefaultStaleTaskTTL},
		{name: "omitted", content: "[workflow]\nresult_checks = \"warn\"\n", want: DefaultStaleTaskTTL},
		{name: "empty", content: "[workflow]\nstale_task_ttl = \"\"\n", want: DefaultStaleTaskTTL},
		{name: "disabled", content: "[workflow]\nstale_task_ttl = \"0\"\n", want: 0},
		{name: "duration", content: "[workflow]\nstale_task_ttl = \"36h\"\n", want: 36 * time.Hour},
		{name: "invalid", content: "[workflow]\nstale_task_ttl = \"later\"\n", wantErr: true},
		{name: "negative", content: "[workflow]\nstale_task_ttl = \"-1h\"\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if test.name != "missing" {
				if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadStaleTaskTTL(Paths{ConfigFile: path})
			if (err != nil) != test.wantErr {
				t.Fatalf("LoadStaleTaskTTL error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && got != test.want {
				t.Fatalf("LoadStaleTaskTTL = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLoadDelegationWorktreeTTL(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    time.Duration
		wantErr bool
	}{
		{name: "missing config", want: DefaultDelegationWorktreeTTL},
		{name: "omitted", content: "[workflow]\nresult_checks = \"warn\"\n", want: DefaultDelegationWorktreeTTL},
		{name: "empty", content: "[workflow]\ndelegation_worktree_ttl = \"\"\n", want: DefaultDelegationWorktreeTTL},
		{name: "disabled", content: "[workflow]\ndelegation_worktree_ttl = \"0\"\n", want: 0},
		{name: "duration", content: "[workflow]\ndelegation_worktree_ttl = \"24h\"\n", want: 24 * time.Hour},
		{name: "invalid", content: "[workflow]\ndelegation_worktree_ttl = \"later\"\n", wantErr: true},
		{name: "negative", content: "[workflow]\ndelegation_worktree_ttl = \"-1h\"\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := Paths{ConfigFile: filepath.Join(t.TempDir(), ConfigName)}
			if tc.content != "" {
				if err := os.WriteFile(paths.ConfigFile, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadDelegationWorktreeTTL(paths)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadDelegationWorktreeTTL() = %s, want error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("LoadDelegationWorktreeTTL() = %s, %v; want %s, nil", got, err, tc.want)
			}
		})
	}
}

func TestLoadPlannedTaskTTLOptIn(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    time.Duration
	}{
		{name: "missing"},
		{name: "omitted", content: "[workflow]\nstale_task_ttl = \"168h\"\n"},
		{name: "empty", content: "[workflow]\nplanned_ttl = \"\"\n"},
		{name: "zero", content: "[workflow]\nplanned_ttl = \"0\"\n"},
		{name: "invalid", content: "[workflow]\nplanned_ttl = \"later\"\n"},
		{name: "negative", content: "[workflow]\nplanned_ttl = \"-1h\"\n"},
		{name: "duration", content: "[workflow]\nplanned_ttl = \"720h\"\n", want: 720 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if test.name != "missing" {
				if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadPlannedTaskTTL(Paths{ConfigFile: path})
			if err != nil || got != test.want {
				t.Fatalf("LoadPlannedTaskTTL = %v, err=%v; want %v", got, err, test.want)
			}
		})
	}
}
