package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/agyaddon"
	"github.com/gitmoot/gitmoot/internal/config"
)

const pluginAgy = "agy"

// Injectable seams for unit tests.
var (
	agyHookInput    io.Reader = os.Stdin
	agyFindAncestor           = func() (int, uint64, error) { return agyaddon.FindAncestorAgyPID(os.Getppid()) }
	agyVerifyPort             = agyaddon.VerifyPIDOwnsLoopbackPort
	agyVerifyMeta   agyaddon.MetadataVerifier
)

type agyPluginLocation struct {
	paths       config.Paths
	pluginDir   string
	registryDir string
	gitmootBin  string
}

func parseAgyPluginArgs(args []string, fs *flag.FlagSet, commandName string, stderr io.Writer) (agyPluginLocation, bool, bool) {
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		return agyPluginLocation{}, false, errors.Is(err, flag.ErrHelp)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept extra positional arguments\n", commandName)
		return agyPluginLocation{}, false, false
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return agyPluginLocation{}, false, false
	}
	pluginDir, err := agyaddon.PluginDir(filepath.Dir(paths.Home), os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return agyPluginLocation{}, false, false
	}
	return agyPluginLocation{
		paths:       paths,
		pluginDir:   pluginDir,
		registryDir: paths.AgyRuntimeDir(),
		gitmootBin:  resolveGitmootBinary(),
	}, true, false
}

func runPluginInstallAgy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin install agy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	loc, ok, help := parseAgyPluginArgs(args, fs, "plugin install agy", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}
	path, changed, err := agyaddon.Install(loc.pluginDir, loc.registryDir, loc.gitmootBin)
	if err != nil {
		fmt.Fprintf(stderr, "plugin install agy: %v\n", err)
		return 1
	}
	writeLine(stdout, "plugin: %s", path)
	writeLine(stdout, "registry: %s", loc.registryDir)
	writeLine(stdout, "version: %s", agyaddon.Version)
	if !changed {
		writeLine(stdout, "agy plugin already up to date")
		return 0
	}
	writeLine(stdout, "installed agy plugin")
	writeLine(stdout, "Running agy seats register automatically on their next gitmoot command (or run `gitmoot plugin agy-register`).")
	return 0
}

func runPluginPathAgy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin path agy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	loc, ok, help := parseAgyPluginArgs(args, fs, "plugin path agy", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}
	writeLine(stdout, "%s", loc.pluginDir)
	return 0
}

func runPluginDoctorAgy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin doctor agy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "print doctor output as JSON")
	loc, ok, help := parseAgyPluginArgs(args, fs, "plugin doctor agy", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	runtime := pluginDoctorRuntime{Runtime: pluginAgy, Path: loc.pluginDir}
	runtime.Checks = append(runtime.Checks, checkHome(loc.paths.Home), checkAgyPlugin(loc), checkAgyRegistryDir(loc.registryDir))
	agyPath, err := pluginLookPath(pluginAgy)
	if err != nil {
		runtime.Checks = append(runtime.Checks, failCheck("runtime-cli", "agy was not found on PATH", true))
	} else {
		runtime.Checks = append(runtime.Checks, okCheck("runtime-cli", agyPath, true))
	}
	runtime.Healthy = runtimeChecksHealthy(runtime.Checks)

	output := pluginDoctorOutput{Home: loc.paths.Home, Runtimes: []pluginDoctorRuntime{runtime}}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "write plugin doctor json: %v\n", err)
			return 1
		}
	} else {
		printPluginDoctor(stdout, output)
	}
	if !runtime.Healthy {
		fmt.Fprintln(stderr, "plugin doctor: agy runtime is unhealthy")
		return 1
	}
	return 0
}

func checkAgyPlugin(loc agyPluginLocation) pluginCheck {
	inspection, err := agyaddon.Inspect(loc.pluginDir, loc.registryDir, loc.gitmootBin)
	if err != nil {
		return failCheck("addon", err.Error(), true)
	}
	switch inspection.Status {
	case agyaddon.StatusInstalled:
		return okCheck("addon", fmt.Sprintf("installed, version %s", inspection.Version), true)
	case agyaddon.StatusMissing:
		return failCheck("addon", "missing; run gitmoot plugin install agy", true)
	default:
		installed := inspection.Version
		if installed == "" {
			installed = "unknown"
		}
		detail := fmt.Sprintf("outdated: installed version %s, this gitmoot installs %s", installed, inspection.ExpectedVersion)
		if inspection.RegistryDir != "" && inspection.RegistryDir != inspection.ExpectedRegistryDir {
			detail += fmt.Sprintf("; installed for registry %q, expected %q", inspection.RegistryDir, inspection.ExpectedRegistryDir)
		}
		return failCheck("addon", detail+"; run gitmoot plugin install agy", true)
	}
}

func checkAgyRegistryDir(dir string) pluginCheck {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return okCheck("registry-dir", dir+" (created by the first agy session)", false)
	case err != nil:
		return warnCheck("registry-dir", err.Error(), false)
	case !info.IsDir():
		return failCheck("registry-dir", dir+" is not a directory", true)
	case info.Mode().Perm() != 0o700:
		return warnCheck("registry-dir", fmt.Sprintf("%s has mode %v; registration resets it to 0700", dir, info.Mode().Perm()), false)
	}
	return okCheck("registry-dir", dir, false)
}

func runPluginAgyRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin agy-register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "plugin agy-register does not accept positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "plugin agy-register: %v\n", err)
		return 1
	}
	wrote, err := agyaddon.RegisterFromEnv(context.Background(), agyaddon.RegisterOptions{
		RegistryDir:    paths.AgyRuntimeDir(),
		FindAncestor:   agyFindAncestor,
		VerifyPort:     agyVerifyPort,
		VerifyMetadata: agyVerifyMeta,
		PruneDead:      true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "plugin agy-register: %v\n", err)
		return 1
	}
	if wrote {
		writeLine(stdout, "registered agy session in %s", paths.AgyRuntimeDir())
	} else if strings.TrimSpace(os.Getenv(agyaddon.EnvLSAddress)) == "" {
		writeLine(stdout, "not inside an agy run_command session; nothing registered")
	} else {
		writeLine(stdout, "agy session registration already up to date")
	}
	return 0
}

// runPluginAgyHook handles Antigravity SessionStart and PreInvocation hook
// calls. It updates <registry>/<pid>.conv.json when the owning agy process is
// already registered, and always prints `{}` and exits 0 so hooks never block.
func runPluginAgyHook(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin agy-hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	registryDir := fs.String("registry-dir", "", "explicit agy runtime registry directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	dir := strings.TrimSpace(*registryDir)
	if dir == "" {
		if paths, err := pathsFromFlag(*home); err == nil {
			dir = paths.AgyRuntimeDir()
		}
	}

	var payload struct {
		ConversationID string `json:"conversationId"`
		HookEventName  string `json:"hookEventName"`
	}
	if agyHookInput != nil {
		if raw, err := io.ReadAll(io.LimitReader(agyHookInput, agyaddon.MaxFileBytes)); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &payload)
		}
	}
	convID := strings.TrimSpace(payload.ConversationID)
	if convID == "" {
		convID = strings.TrimSpace(os.Getenv(agyaddon.EnvConversationID))
	}
	if dir != "" && convID != "" {
		if pid, startTime, err := agyFindAncestor(); err == nil {
			_, _ = agyaddon.RecordHookConversation(dir, pid, startTime, convID, payload.HookEventName, time.Now())
		}
	}
	_, _ = io.WriteString(stdout, "{}\n")
	return 0
}

// MaybeRegisterAgyFromEnv performs best-effort, silent opportunistic
// registration when `gitmoot` is executed from inside an interactive `agy`
// `run_command` subprocess. It is called from cmd/gitmoot/main.go (never from
// unit-test cli.Run calls) and respects any `--home` flag in args.
func MaybeRegisterAgyFromEnv(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "-h" || arg == "--help" {
			return
		}
	}
	if strings.TrimSpace(os.Getenv(agyaddon.EnvLSAddress)) == "" ||
		strings.TrimSpace(os.Getenv(agyaddon.EnvCSRFToken)) == "" ||
		strings.TrimSpace(os.Getenv(agyaddon.EnvConversationID)) == "" {
		return
	}
	if len(args) >= 2 && args[0] == "plugin" && (args[1] == "agy-register" || args[1] == "agy-hook") {
		return
	}
	home := extractHomeArg(args)
	paths, err := pathsFromFlag(home)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = agyaddon.RegisterFromEnv(ctx, agyaddon.RegisterOptions{
		RegistryDir: paths.AgyRuntimeDir(),
		PruneDead:   true,
	})
}

func extractHomeArg(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--home" || arg == "-home" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if v, ok := strings.CutPrefix(arg, "--home="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(arg, "-home="); ok {
			return v
		}
	}
	return ""
}
