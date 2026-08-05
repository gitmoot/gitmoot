package cli

import "github.com/gitmoot/gitmoot/internal/config"

// resolvePermissionPolicyObservationEnabled is read-only and off by default.
// A missing, malformed, or explicitly false key leaves the daemon's poll path
// byte-identical; deployment must explicitly set the key true.
func resolvePermissionPolicyObservationEnabled(home string) bool {
	configFile := resolveConfigFile(home)
	if configFile == "" {
		return false
	}
	cfg, err := config.LoadDaemonRuntimeConfig(config.Paths{ConfigFile: configFile})
	return err == nil && cfg.PermissionPolicyObservationEnabledSet && cfg.PermissionPolicyObservationEnabled
}
