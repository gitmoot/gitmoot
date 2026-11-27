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
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/agyaddon"
	"github.com/gitmoot/gitmoot/internal/config"
)

const pluginAgy = "agy"

// Injectable seams for unit tests.
var (
	agyHookInput      io.Reader = os.Stdin
	agyFindAncestor             = func() (int, uint64, error) { return agyaddon.FindAncestorAgyPID(os.Getppid()) }
	agyVerifyPort               = agyaddon.VerifyPIDOwnsLoopbackPort
	agyVerifyMeta     agyaddon.MetadataVerifier
	agyPlatformCheck            = agyaddon.PlatformSupported
	agyAutoRegisterMu sync.Mutex
	agyAutoRegDone    bool
)

// maybeRegisterAgyForHome performs best-effort opportunistic registration once
// per CLI process after a command has validated its request and selected its
// home. Skipped in go test binaries so tests never touch the operator's home.
func maybeRegisterAgyForHome(home string) {
	if testing.Testing() {
		return
	}
	if strings.TrimSpace(os.Getenv(agyaddon.EnvLSAddress)) == "" ||
		strings.TrimSpace(os.Getenv(agyaddon.EnvCSRFToken)) == "" ||
		strings.TrimSpace(os.Getenv(agyaddon.EnvConversationID)) == "" {
		return
	}
	agyAutoRegisterMu.Lock()
	if agyAutoRegDone {
		agyAutoRegisterMu.Unlock()
		return
	}
	agyAutoRegDone = true
	agyAutoRegisterMu.Unlock()

	paths, err := pathsFromFlag(home)
	if err != nil {
		return
	}
	registryDir, err := absAgyRegistryDir(paths)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = agyaddon.RegisterFromEnv(ctx, agyaddon.RegisterOptions{
		RegistryDir: registryDir,
		PruneDead:   true,
	})
}

func absAgyRegistryDir(paths config.Paths) (string, error) {
	return filepath.Abs(paths.AgyRuntimeDir())
}

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
	absHome, err := filepath.Abs(paths.Home)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return agyPluginLocation{}, false, false
	}
	paths.Home = absHome
	registryDir, err := absAgyRegistryDir(paths)
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
		registryDir: registryDir,
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
	runtime.Checks = append(runtime.Checks, checkHome(loc.paths.Home), checkAgyPlatform(), checkAgyPlugin(loc), checkAgyRegistryDir(loc.registryDir))
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

func checkAgyPlatform() pluginCheck {
	if err := agyPlatformCheck(); err != nil {
		return failCheck("platform", err.Error(), true)
	}
	return okCheck("platform", "linux procfs process/socket inspection available", true)
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
	registryDir, err := absAgyRegistryDir(paths)
	if err != nil {
		fmt.Fprintf(stderr, "plugin agy-register: %v\n", err)
		return 1
	}
	wrote, err := agyaddon.RegisterFromEnv(context.Background(), agyaddon.RegisterOptions{
		RegistryDir:    registryDir,
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
		writeLine(stdout, "registered agy session in %s", registryDir)
	} else if strings.TrimSpace(os.Getenv(agyaddon.EnvLSAddress)) == "" {
		writeLine(stdout, "not inside an agy run_command session; nothing registered")
	} else {
		writeLine(stdout, "agy session registration already up to date")
	}
	return 0
}

// runPluginAgyHook handles Antigravity SessionStart and PreInvocation hook
// calls. It updates <registry>/<pid>.conv.json only when the owning agy process
// is already registered and the conversation is verified as an interactive
// top-level conversation (nestingDepth == 0). Always prints `{}` and exits 0.
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
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	} else if paths, err := pathsFromFlag(*home); err == nil {
		if abs, err := absAgyRegistryDir(paths); err == nil {
			dir = abs
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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = agyaddon.RecordHookConversation(ctx, agyaddon.HookConversationOptions{
				RegistryDir:    dir,
				PID:            pid,
				StartTimeTicks: startTime,
				ConversationID: convID,
				Event:          payload.HookEventName,
				VerifyPort:     agyVerifyPort,
				VerifyMetadata: agyVerifyMeta,
			})
			cancel()
		}
	}
	_, _ = io.WriteString(stdout, "{}\n")
	return 0
}
