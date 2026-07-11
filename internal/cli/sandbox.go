package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/sandbox"
)

func runSandbox(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "sandbox requires a subcommand: probe")
		return 2
	}
	switch args[0] {
	case "probe":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "sandbox probe does not accept arguments")
			return 2
		}
		result := sandbox.SandboxProbe()
		if result.Supported {
			fmt.Fprintf(stdout, "supported (Landlock ABI v%d)\n", result.ABI)
			return 0
		}
		detail := "enforcement self-test failed"
		if result.Err != nil {
			detail = result.Err.Error()
		}
		if result.ABI > 0 {
			fmt.Fprintf(stdout, "unsupported (Landlock ABI v%d): %s\n", result.ABI, detail)
		} else {
			fmt.Fprintf(stdout, "unsupported: %s\n", detail)
		}
		return 1
	default:
		fmt.Fprintf(stderr, "unknown sandbox subcommand %q; use probe\n", args[0])
		return 2
	}
}

type sandboxWriteFlags []string

func (v *sandboxWriteFlags) String() string { return strings.Join(*v, ",") }

func (v *sandboxWriteFlags) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*v = append(*v, value)
	return nil
}

// runSandboxExec is intentionally hidden from root usage. It is an internal
// re-exec boundary used by WrappingRunner, not an operator workflow.
func runSandboxExec(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var writes sandboxWriteFlags
	fs.Var(&writes, "write", "absolute directory writable by the sandbox (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "sandbox-exec requires -- <command> [args...]")
		return 2
	}
	if err := sandbox.Exec([]string(writes), argv); err != nil {
		fmt.Fprintf(stderr, "sandbox-exec: %v\n", err)
		return 1
	}
	return 0
}
