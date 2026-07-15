package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

func TestPipelineEnvParseAndResolve(t *testing.T) {
	values, err := ParseEnv("/tmp/pipeline.env", []byte("# comment\nexport REDDIT_ID='one'\nREDDIT_SECRET=two\nPLAIN=three\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, map[string]string{"REDDIT_ID": "one", "REDDIT_SECRET": "two", "PLAIN": "three"}) {
		t.Fatalf("values = %#v", values)
	}
	keys, err := ResolveEnvKeys([]string{"REDDIT_*", "PLAIN", "REDDIT_ID"}, values)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"REDDIT_ID", "REDDIT_SECRET", "PLAIN"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %#v, want %#v", keys, want)
	}

	const secret = "never-print-this-value"
	_, err = ParseEnv("/tmp/pipeline.env", []byte("BROKEN "+secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error = %v, want redacted malformed-line error", err)
	}
}

func TestPipelineEnvSpecValidation(t *testing.T) {
	tests := []struct {
		name, extra, stage, want string
	}{
		{name: "relative file", extra: "env_file: relative.env\n", stage: "{id: run, cmd: echo}", want: "must be absolute"},
		{name: "reserved inline", extra: "env: {GITMOOT_PIPELINE_NAME: bad}\n", stage: "{id: run, cmd: echo}", want: "reserved GITMOOT_*"},
		{name: "reserved selector", stage: "{id: run, cmd: echo, env_keys: [GITMOOT_*]}", want: "reserved GITMOOT_*"},
		{name: "agent denied", stage: "{id: run, agent: scout, action: ask, prompt: inspect, env_keys: [TOKEN]}", want: "agent and gate stages receive no injected environment"},
		{name: "valid shell", extra: "env_file: /tmp/pipeline.env\nenv: {DATA_DIR: /tmp/data}\n", stage: "{id: run, cmd: echo, env_keys: [API_*, DATA_DIR]}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "name: env-flow\n" + tt.extra + "stages:\n  - " + tt.stage + "\n"
			spec, err := Load([]byte(raw))
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Load error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if spec.EnvFile != "/tmp/pipeline.env" || spec.Env["DATA_DIR"] != "/tmp/data" || !reflect.DeepEqual(spec.Stages[0].EnvKeys, []string{"API_*", "DATA_DIR"}) {
				t.Fatalf("parsed spec = %+v", spec)
			}
		})
	}
}
