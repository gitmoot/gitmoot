// Command clidocgen regenerates the skill's CLI reference
// (skills/gitmoot/references/CLI.md) from the command tree's own `--help`
// output, so the reference cannot drift from the binary. It runs from
// `go generate ./...` (directive in skills/assets.go); CI re-runs that and
// fails on `git diff --exit-code` when the checked-in file is stale.
//
// Help is collected in-process through cli.Run against a throwaway HOME with
// the org-role and Herdr environment cleared, so neither the operator's real
// gitmoot home nor their session identity can reach the output.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/gitmoot/gitmoot/internal/cli"
	"github.com/gitmoot/gitmoot/internal/clidoc"
)

func main() {
	out := flag.String("out", "", "path of the CLI reference to write")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "clidocgen: -out is required")
		os.Exit(2)
	}
	if err := generate(*out); err != nil {
		fmt.Fprintf(os.Stderr, "clidocgen: %v\n", err)
		os.Exit(1)
	}
}

func generate(out string) error {
	home, err := os.MkdirTemp("", "gitmoot-clidocgen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	set, unset := clidoc.Isolation(home, os.Environ())
	for key, value := range set {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	for _, key := range unset {
		if err := os.Unsetenv(key); err != nil {
			return err
		}
	}
	tree, err := clidoc.Discover(func(args []string) (string, int) {
		var buf bytes.Buffer
		code := cli.Run(args, &buf, &buf)
		return buf.String(), code
	})
	if err != nil {
		return err
	}
	return os.WriteFile(out, clidoc.Render(tree), 0o644)
}
