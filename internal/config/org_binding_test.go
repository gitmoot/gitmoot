package config

import (
	"context"
	"testing"
)

func TestResolveRolePaneBinding(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		binding     string
		resolutions map[string]string
		wantPane    string
		wantOK      bool
	}{
		{name: "empty", binding: " \t ", wantPane: "", wantOK: false},
		{name: "label resolves", binding: " coordinator ", resolutions: map[string]string{"coordinator": "w2:p5"}, wantPane: "w2:p5", wantOK: true},
		{name: "unknown label is literal id", binding: "w1:p2", wantPane: "w1:p2", wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pane, ok := ResolveRolePaneBinding(ctx, test.binding, func(_ context.Context, label string) (string, bool) {
				resolved, found := test.resolutions[label]
				return resolved, found
			})
			if pane != test.wantPane || ok != test.wantOK {
				t.Fatalf("ResolveRolePaneBinding(%q) = (%q, %t), want (%q, %t)", test.binding, pane, ok, test.wantPane, test.wantOK)
			}
		})
	}
}
