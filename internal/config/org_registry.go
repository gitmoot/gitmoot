package config

import (
	"fmt"
	"os"

	"github.com/gitmoot/gitmoot/internal/org"
)

// LoadOrgRegistry is the canonical phase-1b loader. A future #1057 LoadOrg
// should reconcile by wrapping this function rather than introducing a second
// parser or policy model.
func LoadOrgRegistry(paths Paths) (org.Registry, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return org.Registry{}, nil
		}
		return org.Registry{}, err
	}
	registry, err := org.ParseRegistry(content)
	if err != nil {
		return org.Registry{}, fmt.Errorf("load org registry: %w", err)
	}
	return registry, nil
}
