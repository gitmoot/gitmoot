package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

func runInteractive(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printInteractiveUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runInteractiveList(args[1:], stdout, stderr)
	case "show":
		return runInteractiveShow(args[1:], stdout, stderr)
	case "answer":
		return runInteractiveAnswer(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown interactive command %q\n\n", args[0])
		printInteractiveUsage(stderr)
		return 2
	}
}

func printInteractiveUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot interactive list [--state pending|resolved|all] [--json]")
	fmt.Fprintln(w, "  gitmoot interactive show <id> --json")
	fmt.Fprintln(w, "  gitmoot interactive answer <id> <value> [--source source]")
}

func runInteractiveList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("interactive list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	state := fs.String("state", db.InteractivePromptStatePending, "prompt state: pending, resolved, or all")
	jsonOutput := fs.Bool("json", false, "write prompts as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "interactive list does not accept positional arguments")
		return 2
	}
	normalizedState, err := normalizeInteractivePromptState(*state)
	if err != nil {
		fmt.Fprintf(stderr, "interactive list: %v\n", err)
		return 2
	}
	var prompts []db.InteractivePrompt
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		prompts, err = store.ListInteractivePrompts(context.Background(), normalizedState)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "interactive list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, prompts); err != nil {
			fmt.Fprintf(stderr, "interactive list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, prompt := range prompts {
		writeLine(stdout, "%s\t%s\t%s", prompt.ID, prompt.State, prompt.Question)
	}
	return 0
}

func runInteractiveShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("interactive show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "write prompt as JSON")
	parsedArgs, err := reorderInteractiveFlags(args, map[string]struct{}{"home": {}}, map[string]struct{}{"json": {}})
	if err != nil {
		fmt.Fprintf(stderr, "interactive show: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "interactive show requires exactly one prompt id")
		return 2
	}
	if !*jsonOutput {
		fmt.Fprintln(stderr, "interactive show requires --json")
		return 2
	}
	var prompt db.InteractivePrompt
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		prompt, err = store.GetInteractivePrompt(context.Background(), fs.Arg(0))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "interactive show: %v\n", err)
		return 1
	}
	if err := writeJSON(stdout, prompt); err != nil {
		fmt.Fprintf(stderr, "interactive show: %v\n", err)
		return 1
	}
	return 0
}

func runInteractiveAnswer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("interactive answer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	source := fs.String("source", "cli", "answer source")
	parsedArgs, err := reorderInteractiveFlags(args, map[string]struct{}{"home": {}, "source": {}}, nil)
	if err != nil {
		fmt.Fprintf(stderr, "interactive answer: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "interactive answer requires a prompt id and value")
		return 2
	}
	var prompt db.InteractivePrompt
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		prompt, err = store.AnswerInteractivePrompt(context.Background(), fs.Arg(0), fs.Arg(1), *source)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "interactive answer: %v\n", err)
		return 1
	}
	writeLine(stdout, "answered %s: %s", prompt.ID, prompt.AnswerValue)
	return 0
}

func normalizeInteractivePromptState(state string) (string, error) {
	state = strings.TrimSpace(strings.ToLower(state))
	switch state {
	case "", "all":
		return "", nil
	case db.InteractivePromptStatePending, db.InteractivePromptStateResolved:
		return state, nil
	default:
		return "", fmt.Errorf("state %q is not supported", state)
	}
}

func reorderInteractiveFlags(args []string, stringFlags map[string]struct{}, boolFlags map[string]struct{}) ([]string, error) {
	flags := []string{}
	positionals := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name, hasInlineValue := splitInteractiveFlagName(arg)
		if _, ok := boolFlags[name]; ok {
			flags = append(flags, arg)
			continue
		}
		if _, ok := stringFlags[name]; ok {
			flags = append(flags, arg)
			if !hasInlineValue {
				if index+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		flags = append(flags, arg)
	}
	return append(flags, positionals...), nil
}

func splitInteractiveFlagName(arg string) (string, bool) {
	arg = strings.TrimLeft(arg, "-")
	name, value, hasInlineValue := strings.Cut(arg, "=")
	_ = value
	return name, hasInlineValue
}
