package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/sandbox"
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

type sandboxPathFlags []string

func (v *sandboxPathFlags) String() string { return strings.Join(*v, ",") }

func (v *sandboxPathFlags) Set(value string) error {
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
	var reads, readFiles, writes sandboxPathFlags
	readOnlyWorkdir := fs.Bool("read-only-workdir", false, "deny writes to the sandbox working directory")
	noExecution := fs.Bool("no-execution", false, "allow starting only the target runtime; refuse every later exec (implies --read-only-workdir)")
	noExecutionChild := fs.Bool("no-execution-child", false, "internal: confined child of --no-execution")
	fs.Var(&reads, "read", "absolute directory readable by the sandbox (repeatable)")
	fs.Var(&readFiles, "read-file", "absolute file readable by the sandbox (repeatable)")
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
	if *noExecution && !*noExecutionChild {
		// Supervisor: re-run this command as the confined child and stay as its
		// parent to refuse every exec after the runtime starts.
		self, selfErr := os.Executable()
		if selfErr != nil {
			fmt.Fprintf(stderr, "sandbox-exec: resolve self: %v\n", selfErr)
			return 1
		}
		childArgs := append([]string{"sandbox-exec", "--no-execution-child"}, sandboxPathArgs(reads, readFiles, writes)...)
		childArgs = append(append(childArgs, "--"), argv...)
		code, superviseErr := sandbox.SuperviseNoExecution(self, childArgs)
		if superviseErr != nil {
			fmt.Fprintf(stderr, "sandbox-exec: %v\n", superviseErr)
		}
		return code
	}
	var err error
	switch {
	case *noExecutionChild:
		err = sandbox.ExecNoExecutionChild([]string(reads), []string(readFiles), []string(writes), argv)
	case *readOnlyWorkdir:
		err = sandbox.ExecReadOnlyWorkdir([]string(reads), []string(readFiles), []string(writes), argv)
	default:
		err = sandbox.Exec([]string(reads), []string(readFiles), []string(writes), argv)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sandbox-exec: %v\n", err)
		return 1
	}
	return 0
}

func sandboxPathArgs(reads, readFiles, writes sandboxPathFlags) []string {
	var args []string
	for _, path := range reads {
		args = append(args, "--read", path)
	}
	for _, path := range readFiles {
		args = append(args, "--read-file", path)
	}
	for _, path := range writes {
		args = append(args, "--write", path)
	}
	return args
}
