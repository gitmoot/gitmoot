package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/jerryfane/gitmoot/internal/cli/style"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

var authReadSecret = func(prompt io.Writer) (string, error) {
	if style.IsTerminal(os.Stdin) {
		fmt.Fprint(prompt, "Claude token: ")
		value, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(prompt)
		return string(value), err
	}
	value, err := io.ReadAll(io.LimitReader(os.Stdin, runtimeAuthMaxValueBytes+2))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r"), nil
}

func runAuth(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printAuthUsage(stdout)
		return 0
	}
	switch args[0] {
	case "set":
		return runAuthSet(args[1:], stdout, stderr)
	case "unset":
		return runAuthUnset(args[1:], stdout, stderr)
	case "status":
		return runAuthStatus(args[1:], stdout, stderr)
	case "probe":
		return runAuthProbe(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown auth command %q\n\n", args[0])
		printAuthUsage(stderr)
		return 2
	}
}

func printAuthUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot auth set claude [--var NAME] [--from-env]")
	fmt.Fprintln(w, "  gitmoot auth unset [claude]")
	fmt.Fprintln(w, "  gitmoot auth status")
	fmt.Fprintln(w, "  gitmoot auth probe [claude]")
}

func authProviderArgs(command string, args []string, optional bool, stderr io.Writer) ([]string, int) {
	if len(args) > 0 && args[0] == "claude" {
		return args[1:], 0
	}
	if optional && (len(args) == 0 || strings.HasPrefix(args[0], "-")) {
		return args, 0
	}
	fmt.Fprintf(stderr, "%s supports only the claude provider\n", command)
	return nil, 2
}

func runAuthSet(args []string, stdout, stderr io.Writer) int {
	args, code := authProviderArgs("auth set", args, false, stderr)
	if code != 0 {
		return code
	}
	fs := flag.NewFlagSet("auth set claude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	name := fs.String("var", runtime.ClaudeOAuthTokenEnv, "managed Claude auth variable")
	fromEnv := fs.Bool("from-env", false, "copy ambient managed Claude auth variables")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "auth set claude does not accept positional arguments")
		return 2
	}
	if !managedRuntimeAuthVar(*name) {
		fmt.Fprintf(stderr, "auth set claude: --var must be one of %s\n", strings.Join(runtimeAuthEnvVars, ", "))
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "auth set claude: %v\n", err)
		return 1
	}
	values := map[string]string{}
	if *fromEnv {
		values = collectRuntimeAuthEnv(runtimeAuthEnvLookup)
		if len(values) == 0 {
			fmt.Fprintln(stderr, "auth set claude: no managed Claude auth variables are set in the ambient environment")
			return 1
		}
	} else {
		value, err := authReadSecret(stderr)
		if err != nil {
			fmt.Fprintf(stderr, "auth set claude: read token: %v\n", err)
			return 1
		}
		if err := validateRuntimeAuthValue(value); err != nil {
			fmt.Fprintf(stderr, "auth set claude: invalid %s: %v\n", *name, err)
			return 1
		}
		values[*name] = value
	}
	if oauth, ok := values[runtime.ClaudeOAuthTokenEnv]; ok && !strings.HasPrefix(oauth, "sk-ant-oat01-") {
		fmt.Fprintln(stderr, "WARNING: token does not look like a long-lived Claude setup token (expected sk-ant-oat01- prefix)")
	}
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), values); err != nil {
		fmt.Fprintf(stderr, "auth set claude: %v\n", err)
		return 1
	}
	writeLine(stdout, "Claude runtime auth updated at %s", runtimeAuthFilePath(paths.Home))
	for _, envName := range sortedRuntimeAuthNames(values) {
		writeLine(stdout, "%s=%s", envName, maskedAuthFingerprint(values[envName]))
	}
	return 0
}

func runAuthUnset(args []string, stdout, stderr io.Writer) int {
	args, code := authProviderArgs("auth unset", args, true, stderr)
	if code != 0 {
		return code
	}
	fs := flag.NewFlagSet("auth unset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "auth unset accepts only the optional claude provider")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "auth unset: %v\n", err)
		return 1
	}
	if err := writeRuntimeAuthFile(runtimeAuthFilePath(paths.Home), nil); err != nil {
		fmt.Fprintf(stderr, "auth unset: %v\n", err)
		return 1
	}
	writeLine(stdout, "Claude runtime auth unset; wrote explicit empty %s", runtimeAuthFilePath(paths.Home))
	return 0
}

func runAuthStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "auth status does not accept positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "auth status: %v\n", err)
		return 1
	}
	path := runtimeAuthFilePath(paths.Home)
	writeLine(stdout, "Claude runtime auth")
	writeLine(stdout, "file: %s", path)
	state, err := loadRuntimeAuthFile(paths.Home)
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			writeLine(stdout, "file status: present (permissions %04o, modified %s)", info.Mode().Perm(), info.ModTime().UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(stderr, "WARNING: refusing to display runtime auth status: %v\n", err)
		return 1
	}
	if state.Exists {
		writeLine(stdout, "file status: present (permissions %04o, modified %s)", state.Mode, state.ModTime.UTC().Format(time.RFC3339))
		if len(state.Values) == 0 {
			writeLine(stdout, "file values: explicit empty")
		} else {
			for _, envName := range sortedRuntimeAuthNames(state.Values) {
				writeLine(stdout, "file %s=%s", envName, maskedAuthFingerprint(state.Values[envName]))
			}
		}
	} else {
		writeLine(stdout, "file status: missing")
	}
	ambient := collectRuntimeAuthEnv(runtimeAuthEnvLookup)
	if len(ambient) == 0 {
		writeLine(stdout, "ambient: none")
	} else {
		for _, envName := range sortedRuntimeAuthNames(ambient) {
			writeLine(stdout, "ambient %s=%s", envName, maskedAuthFingerprint(ambient[envName]))
		}
	}
	legacy, legacyExists, legacyErr := loadLegacyRuntimeAuthFile(legacyRuntimeAuthFilePath(paths.Home))
	if legacyErr == nil && legacyExists {
		writeLine(stdout, "legacy file: present")
		if len(legacy) == 0 {
			writeLine(stdout, "legacy values: explicit empty")
		} else {
			for _, envName := range sortedRuntimeAuthNames(legacy) {
				writeLine(stdout, "legacy %s=%s", envName, maskedAuthFingerprint(legacy[envName]))
			}
		}
	} else if legacyErr == nil {
		writeLine(stdout, "legacy file: missing")
	} else {
		writeLine(stdout, "legacy file: unreadable")
		fmt.Fprintf(stderr, "WARNING: %v\n", legacyErr)
	}
	winner := runtimeAuthSource(state, runtimeAuthEnvLookup)
	if !state.Exists && legacyErr != nil {
		writeLine(stdout, "winner: unavailable until the legacy file error is fixed")
		return 1
	}
	if !state.Exists && legacyErr == nil && legacyExists {
		if len(legacy) > 0 {
			winner = legacyRuntimeAuthFileName + " (will import to " + runtimeAuthFileName + ")"
		} else if len(ambient) > 0 {
			winner = "ambient environment (legacy import is explicitly empty)"
		} else {
			winner = "Claude credential store (legacy import is explicitly empty)"
		}
	}
	writeLine(stdout, "winner: %s", winner)
	return 0
}

func runAuthProbe(args []string, stdout, stderr io.Writer) int {
	args, code := authProviderArgs("auth probe", args, true, stderr)
	if code != 0 {
		return code
	}
	fs := flag.NewFlagSet("auth probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "auth probe accepts only the optional claude provider")
		return 2
	}
	runner, _, source, err := runtimeJobRunnerWithAuth(*home, runtime.ClaudeRuntime, nil)
	if err != nil {
		fmt.Fprintf(stderr, "auth probe: %v\n", err)
		return 1
	}
	writeLine(stdout, "probing Claude auth source: %s", source)
	probeErr := runtime.ClaudeLiveCheckEnv(context.Background(), runner, "", nil)
	status := runtime.ClaudeClassifyProbe(probeErr)
	writeLine(stdout, "Claude auth: %s", status.String())
	if probeErr != nil {
		fmt.Fprintf(stderr, "auth probe: %v\n", probeErr)
		return 1
	}
	return 0
}

func sortedRuntimeAuthNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for _, name := range runtimeAuthEnvVars {
		if _, ok := values[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
