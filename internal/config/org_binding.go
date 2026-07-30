package config

import (
	"context"
	"strings"
)

// ResolveRolePaneBinding resolves an OrgRole.Pane binding to a Herdr pane id.
// The live resolver accepts either an exact pane label or a literal pane id.
// Empty and non-live bindings do not resolve.
func ResolveRolePaneBinding(ctx context.Context, binding string, resolveLivePane func(context.Context, string) (string, bool)) (string, bool) {
	binding = strings.TrimSpace(binding)
	if binding == "" {
		return "", false
	}
	if resolved, found := resolveLivePane(ctx, binding); found {
		return resolved, true
	}
	return "", false
}
