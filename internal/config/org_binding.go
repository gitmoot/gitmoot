package config

import (
	"context"
	"strings"
)

// ResolveRolePaneBinding resolves an OrgRole.Pane binding to a Herdr pane id.
// A binding is treated as a pane label first and as a literal pane id when no
// live pane has that label. Empty bindings do not resolve.
func ResolveRolePaneBinding(ctx context.Context, binding string, resolveLabel func(context.Context, string) (string, bool)) (string, bool) {
	binding = strings.TrimSpace(binding)
	if binding == "" {
		return "", false
	}
	if resolved, found := resolveLabel(ctx, binding); found {
		return resolved, true
	}
	return binding, true
}
