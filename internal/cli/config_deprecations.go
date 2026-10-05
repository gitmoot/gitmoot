package cli

import (
	"fmt"
	"io"

	"github.com/gitmoot/gitmoot/internal/config"
)

// printConfigDeprecations prints, once at daemon start, every deprecated
// spelling config.toml still uses, each naming its replacement. A config that
// cannot be loaded prints nothing here: the paths that use it report that.
func printConfigDeprecations(paths config.Paths, stderr io.Writer) {
	var deprecations []string
	if remote, err := config.LoadRemoteExecConfig(paths); err == nil {
		deprecations = append(deprecations, remote.Deprecations...)
	}
	// A review config error is per repository and reported where that
	// repository is used; the deprecations of the rest still apply.
	review, _ := config.LoadReviewConfig(paths)
	deprecations = append(deprecations, review.Deprecations()...)
	for _, deprecation := range deprecations {
		fmt.Fprintf(stderr, "daemon run: warning: %s\n", deprecation)
	}
}
