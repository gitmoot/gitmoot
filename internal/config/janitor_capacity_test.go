package config

import (
	"strings"
	"testing"
	"time"
)

const fixtureJanitorHash = "95a20125287a185d0000000000000000000000000000000000000000000000ab"

// #2359: absent or enabled=false is off; a full section parses exactly.
func TestLoadJanitorCapacityDefaultsAndConfigured(t *testing.T) {
	off, err := LoadJanitorCapacity(writeGateConfig(t, "[workflow]\nrequire_workflow = true\n"))
	if err != nil || off.Enabled {
		t.Fatalf("no section = %+v %v, want disabled", off, err)
	}
	got, err := LoadJanitorCapacity(writeGateConfig(t, "[janitor_capacity]\nenabled = true\nexecutable = \"/root/.local/bin/janitor\"\npolicy_hash = \""+fixtureJanitorHash+"\"\ntimeout_ms = 2500\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := JanitorCapacity{Enabled: true, Executable: "/root/.local/bin/janitor", PolicyHash: fixtureJanitorHash, Timeout: 2500 * time.Millisecond}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	staged, err := LoadJanitorCapacity(writeGateConfig(t, "[janitor_capacity]\nenabled = false\n"))
	if err != nil || staged.Enabled || staged.Timeout != DefaultJanitorCapacityTimeout {
		t.Fatalf("enabled=false = %+v %v", staged, err)
	}
}

// Anything ambiguous is a config error, never a silent "disabled".
func TestLoadJanitorCapacityRefusesInvalidSections(t *testing.T) {
	for name, body := range map[string]string{
		"enabled without executable": "enabled = true\npolicy_hash = \"" + fixtureJanitorHash + "\"\n",
		"enabled without policy":     "enabled = true\nexecutable = \"/root/.local/bin/janitor\"\n",
		"relative executable":        "executable = \"janitor\"\n",
		"short policy hash":          "policy_hash = \"95a20125287a185d\"\n",
		"uppercase policy hash":      "policy_hash = \"" + strings.ToUpper(fixtureJanitorHash) + "\"\n",
		"unquoted executable":        "executable = /root/.local/bin/janitor\n",
		"enabled not a boolean":      "enabled = yes\n",
		"timeout out of range":       "timeout_ms = 10\n",
		"unknown key":                "enable = true\n",
		"duplicate key":              "enabled = false\nenabled = false\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadJanitorCapacity(writeGateConfig(t, "[janitor_capacity]\n"+body)); err == nil {
				t.Fatal("invalid section accepted")
			}
		})
	}
	if _, err := LoadJanitorCapacity(writeGateConfig(t, "[janitor_capacity]\nenabled = false\n[janitor_capacity]\nenabled = false\n")); err == nil {
		t.Fatal("duplicate section accepted")
	}
	if _, err := LoadJanitorCapacity(writeGateConfig(t, "[janitor_capacity\nenabled = true\n")); err == nil {
		t.Fatal("malformed header accepted")
	}
}
