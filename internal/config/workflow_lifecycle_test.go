package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadWorkflowLifecycle(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    time.Duration
		wantErr string
	}{
		{name: "missing config", want: 24 * time.Hour},
		{name: "omitted", content: "[workflow]\nimplement_base = \"HEAD\"\n", want: 24 * time.Hour},
		{name: "custom", content: "[workflow]\nauto_settle_after = \"48h\"\n", want: 48 * time.Hour},
		{name: "zero", content: "[workflow]\nauto_settle_after = \"0\"\n", want: 0},
		{name: "zero duration", content: "[workflow]\nauto_settle_after = \"0s\"\n", want: 0},
		{name: "negative", content: "[workflow]\nauto_settle_after = \"-1h\"\n", wantErr: "must not be negative"},
		{name: "garbage", content: "[workflow]\nauto_settle_after = \"later\"\n", wantErr: "invalid [workflow].auto_settle_after"},
		{name: "other section", content: "[memory]\nauto_settle_after = \"2h\"\n", want: 24 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if test.content != "" {
				if err := os.MkdirAll(paths.Home, 0o700); err != nil {
					t.Fatalf("mkdir home: %v", err)
				}
				if err := os.WriteFile(paths.ConfigFile, []byte(test.content), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			got, err := LoadWorkflowLifecycle(paths)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("LoadWorkflowLifecycle error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got.AutoSettleAfter != test.want {
				t.Fatalf("LoadWorkflowLifecycle = %+v, err=%v, want %s", got, err, test.want)
			}
		})
	}
}

func TestDefaultConfigLoadsWorkflowLifecycle(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	policy, err := LoadWorkflowLifecycle(paths)
	if err != nil || policy.AutoSettleAfter != DefaultWorkflowAutoSettleAfter {
		t.Fatalf("LoadWorkflowLifecycle(DefaultConfig) = %+v, err=%v", policy, err)
	}
	body, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), `# auto_settle_after = "24h"`) {
		t.Fatalf("DefaultConfig missing commented auto_settle_after")
	}
}
