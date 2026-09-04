package plugininstall

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/pluginpack"
)

// TestManualCommandsQuoteCommaAndPercent pins this call site's #1759 behaviour
// change, which was unproven: restoring its own pre-#1759 quoter (a wider
// allowlist admitting ',' and '%') compiled and survived the whole suite.
//
// The observable change is exactly that a marketplaceRoot or packagePath
// containing ',' or '%' is now QUOTED where it previously was not, so that is
// what this asserts - not the quoter's internals.
func TestManualCommandsQuoteCommaAndPercent(t *testing.T) {
	for name, path := range map[string]string{
		"comma":   "/tmp/repo,with,commas",
		"percent": "/tmp/repo%name",
		"both":    "/tmp/a,b%c",
	} {
		t.Run(name, func(t *testing.T) {
			for _, provider := range []pluginpack.Provider{pluginpack.ProviderCodex, pluginpack.ProviderClaude} {
				for _, command := range manualCommands(provider, path, path, "user") {
					if !strings.Contains(command, path) {
						continue
					}
					if strings.Contains(command, "'"+path+"'") {
						continue
					}
					t.Errorf("%s command leaves %q unquoted, so a shell would reinterpret it: %s", provider, path, command)
				}
			}
		})
	}

	// A path needing no quoting must still be emitted bare, so the assertion
	// above cannot be satisfied by quoting everything.
	for _, command := range manualCommands(pluginpack.ProviderCodex, "/tmp/plain", "/tmp/plain", "user") {
		if strings.Contains(command, "'/tmp/plain'") {
			t.Errorf("a safe path was quoted unnecessarily: %s", command)
		}
	}
}
