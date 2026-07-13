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
