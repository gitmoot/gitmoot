package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// JanitorCapacity is the opt-in, default-off pressure admission consumer (#2359).
// When enabled, gitmoot holds NEW heavy work (every queued or foreground agent or
// shell job) while workspace-janitor's typed capacity status does not clear the
// host devices that work would write to. It never cancels running work, and it
// never replaces the [disk_guard] floors, which still apply on their own.
//
//	[janitor_capacity]
//	enabled = true
//	executable = "/root/.local/bin/janitor"   # absolute; run as: <executable> --format json status
//	policy_hash = "<64 hex>"                  # the approved janitor policy; any other hash holds
//	timeout_ms = 5000                         # optional, 100..60000
//
// An absent section, or enabled = false, means no janitor dependency at all: no
// process is started and nothing is read. A present but invalid section is a
// load ERROR, never a silent "disabled".
type JanitorCapacity struct {
	Enabled          bool
	Executable       string
	PolicyHash       string
	Timeout          time.Duration
	ReviewFreshSpace bool
}

// DefaultJanitorCapacityTimeout bounds one status call when timeout_ms is unset.
const DefaultJanitorCapacityTimeout = 5 * time.Second

var (
	janitorCapacityKeys    = []string{"enabled", "executable", "policy_hash", "timeout_ms", "review_admission"}
	janitorPolicyHashShape = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LoadJanitorCapacity parses [janitor_capacity]. A missing file or section is disabled.
func LoadJanitorCapacity(paths Paths) (JanitorCapacity, error) {
	values, err := loadSingleSectionValues(paths, "janitor_capacity")
	if err != nil || values == nil {
		return JanitorCapacity{}, err
	}
	return buildJanitorCapacity(values)
}

func buildJanitorCapacity(values map[string]string) (JanitorCapacity, error) {
	for key := range values {
		known := false
		for _, k := range janitorCapacityKeys {
			known = known || k == key
		}
		if !known {
			return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: unknown key %q", key)
		}
	}
	c := JanitorCapacity{Timeout: DefaultJanitorCapacityTimeout}
	if raw, ok := values["enabled"]; ok {
		switch raw {
		case "true":
			c.Enabled = true
		case "false":
		default:
			return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: enabled must be true or false")
		}
	}
	if raw, ok := values["review_admission"]; ok {
		mode, err := strconv.Unquote(raw)
		if err != nil || (mode != "cleanup_target" && mode != "fresh_space") {
			return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: review_admission must be \"cleanup_target\" or \"fresh_space\"")
		}
		c.ReviewFreshSpace = mode == "fresh_space"
	}
	for _, key := range []string{"executable", "policy_hash"} {
		raw, ok := values[key]
		if !ok {
			continue
		}
		s, err := strconv.Unquote(raw)
		if err != nil || strings.TrimSpace(s) != s || s == "" {
			return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: %s must be a non-empty quoted string", key)
		}
		if key == "executable" {
			c.Executable = s
		} else {
			c.PolicyHash = s
		}
	}
	if raw, ok := values["timeout_ms"]; ok {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 100 || ms > 60000 {
			return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: timeout_ms must be an integer from 100 to 60000")
		}
		c.Timeout = time.Duration(ms) * time.Millisecond
	}
	if c.Executable != "" && (!filepath.IsAbs(c.Executable) || filepath.Clean(c.Executable) != c.Executable) {
		return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: executable %q must be a clean absolute path", c.Executable)
	}
	if c.PolicyHash != "" && !janitorPolicyHashShape.MatchString(c.PolicyHash) {
		return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: policy_hash must be 64 lowercase hex characters")
	}
	if c.Enabled && (c.Executable == "" || c.PolicyHash == "") {
		return JanitorCapacity{}, fmt.Errorf("[janitor_capacity]: enabled = true requires executable and policy_hash")
	}
	return c, nil
}
