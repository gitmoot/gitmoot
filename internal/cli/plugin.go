package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/plugininstall"
	"github.com/jerryfane/gitmoot/internal/pluginpack"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/skills"
)

var pluginLookPath = exec.LookPath
var pluginInstallRunner subprocess.Runner = subprocess.ExecRunner{}

type pluginCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Required bool   `json:"required"`
}

type pluginDoctorRuntime struct {
	Runtime string        `json:"runtime"`
	Path    string        `json:"path"`
	Healthy bool          `json:"healthy"`
	Checks  []pluginCheck `json:"checks"`
}

type pluginDoctorOutput struct {
	Home     string                `json:"home"`
	Runtimes []pluginDoctorRuntime `json:"runtimes"`
}

func runPlugin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPluginUsage(stdout)
		return 0
	}
	switch args[0] {
	case "build":
		return runPluginBuild(args[1:], stdout, stderr)
	case "path":
		return runPluginPath(args[1:], stdout, stderr)
	case "doctor":
		return runPluginDoctor(args[1:], stdout, stderr)
	case "install":
		return runPluginInstall(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown plugin command %q\n\n", args[0])
		printPluginUsage(stderr)
		return 2
	}
}

func printPluginUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot plugin build codex|claude")
	fmt.Fprintln(w, "  gitmoot plugin install codex|claude")
	fmt.Fprintln(w, "  gitmoot plugin path codex|claude")
	fmt.Fprintln(w, "  gitmoot plugin doctor [codex|claude]")
}

func runPluginInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	scope := fs.String("scope", "user", "Claude plugin scope: user, project, or local")
	force := fs.Bool("force", false, "replace existing generated plugin package")
	explicitScope := hasFlag(args, "scope")
	provider, ok, help := parsePluginProviderArg(args, fs, stderr, "plugin install")
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "plugin install: %v\n", err)
		return 1
	}
	result, err := plugininstall.Install(context.Background(), plugininstall.Options{
		Provider: provider,
		Home:     paths.Home,
		Scope:    *scope,
		Force:    *force,
		Info:     buildinfo.Current(),
		Runner:   pluginInstallRunner,
	})
	if err != nil {
		fmt.Fprintf(stderr, "plugin install: %v\n", err)
		return 1
	}
	writeLine(stdout, "package: %s", result.PackagePath)
	writeLine(stdout, "marketplace: %s", result.MarketplaceRoot)
	if provider == pluginpack.ProviderCodex && explicitScope {
		writeLine(stdout, "scope: ignored for codex")
	}
	if result.RuntimeMissing {
		writeLine(stdout, "%s CLI was not found; generated files are ready.", provider)
		printPluginManualCommands(stdout, result.ManualCommands)
		return 0
	}
	writeLine(stdout, "installed %s plugin", provider)
	return 0
}

func hasFlag(args []string, name string) bool {
	short := "-" + name
	long := "--" + name
	for _, arg := range args {
		if arg == short || arg == long || strings.HasPrefix(arg, short+"=") || strings.HasPrefix(arg, long+"=") {
			return true
		}
	}
	return false
}

func printPluginManualCommands(w io.Writer, commands []string) {
	if len(commands) == 0 {
		return
	}
	writeLine(w, "manual install commands:")
	for _, command := range commands {
		writeLine(w, "  %s", command)
	}
}

func runPluginBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	outDir := fs.String("out", "", "package output directory")
	force := fs.Bool("force", false, "replace an existing generated package")
	provider, ok, help := parsePluginProviderArg(args, fs, stderr, "plugin build")
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	homePath := ""
	if *outDir == "" {
		paths, err := pathsFromFlag(*home)
		if err != nil {
			fmt.Fprintf(stderr, "plugin build: %v\n", err)
			return 1
		}
		homePath = paths.Home
	}
	result, err := pluginpack.Build(pluginpack.BuildOptions{
		Provider: provider,
		Home:     homePath,
		OutDir:   *outDir,
		Force:    *force,
		Info:     buildinfo.Current(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "plugin build: %v\n", err)
		return 1
	}
	writeLine(stdout, "%s", result.Path)
	return 0
}

func runPluginPath(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin path", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	provider, ok, help := parsePluginProviderArg(args, fs, stderr, "plugin path")
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "plugin path: %v\n", err)
		return 1
	}
	writeLine(stdout, "%s", pluginpack.DefaultPackageDir(paths.Home, provider))
	return 0
}

func runPluginDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print doctor output as JSON")
	selected, explicitRuntime, help, ok := parsePluginDoctorArgs(args, fs, stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "plugin doctor: %v\n", err)
		return 1
	}
	providers := []pluginpack.Provider{pluginpack.ProviderCodex, pluginpack.ProviderClaude}
	if explicitRuntime {
		providers = []pluginpack.Provider{selected}
	}

	output := pluginDoctorOutput{Home: paths.Home}
	for _, provider := range providers {
		output.Runtimes = append(output.Runtimes, doctorRuntime(paths.Home, provider, explicitRuntime))
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "write plugin doctor json: %v\n", err)
			return 1
		}
	} else {
		printPluginDoctor(stdout, output)
	}

	if explicitRuntime {
		if len(output.Runtimes) == 1 && !output.Runtimes[0].Healthy {
			fmt.Fprintf(stderr, "plugin doctor: %s runtime is unhealthy\n", output.Runtimes[0].Runtime)
			return 1
		}
		return 0
	}
	for _, runtime := range output.Runtimes {
		if runtime.Healthy {
			return 0
		}
	}
	fmt.Fprintln(stderr, "plugin doctor: no supported plugin runtime is healthy")
	return 1
}

func parsePluginProviderArg(args []string, fs *flag.FlagSet, stderr io.Writer, commandName string) (pluginpack.Provider, bool, bool) {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s requires codex|claude\n", commandName)
		return "", false, false
	}
	if args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		return "", false, true
	}
	provider, err := pluginpack.ParseProvider(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return "", false, false
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return provider, false, true
		}
		return "", false, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept extra positional arguments\n", commandName)
		return "", false, false
	}
	return provider, true, false
}

func parsePluginDoctorArgs(args []string, fs *flag.FlagSet, stderr io.Writer) (pluginpack.Provider, bool, bool, bool) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider, err := pluginpack.ParseProvider(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "plugin doctor: %v\n", err)
			return "", false, false, false
		}
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", false, true, false
			}
			return "", false, false, false
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "plugin doctor does not accept extra positional arguments")
			return "", false, false, false
		}
		return provider, true, false, true
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", false, true, false
		}
		return "", false, false, false
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "plugin doctor accepts at most one runtime")
		return "", false, false, false
	}
	if fs.NArg() == 0 {
		return "", false, false, true
	}
	provider, err := pluginpack.ParseProvider(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "plugin doctor: %v\n", err)
		return "", false, false, false
	}
	return provider, true, false, true
}

func doctorRuntime(home string, provider pluginpack.Provider, explicitRuntime bool) pluginDoctorRuntime {
	packagePath := pluginpack.DefaultPackageDir(home, provider)
	runtime := pluginDoctorRuntime{
		Runtime: string(provider),
		Path:    packagePath,
	}

	runtime.Checks = append(runtime.Checks, checkHome(home))
	runtime.Checks = append(runtime.Checks, checkCanonicalSkill())
	runtime.Checks = append(runtime.Checks, checkPackage(packagePath))
	runtime.Checks = append(runtime.Checks, checkManifest(packagePath, provider))
	runtime.Checks = append(runtime.Checks, checkCopiedSkill(packagePath))
	runtime.Checks = append(runtime.Checks, checkMarketplacePath(home, provider))
	runtime.Checks = append(runtime.Checks, checkRuntimeCLI(provider, explicitRuntime))
	if provider == pluginpack.ProviderClaude {
		runtime.Checks = append(runtime.Checks, checkClaudeAuthEnv())
	}
	runtime.Checks = append(runtime.Checks, checkValidationCommand(provider, explicitRuntime))
	runtime.Healthy = runtimeChecksHealthy(runtime.Checks)
	return runtime
}

func checkHome(home string) pluginCheck {
	if home == "" {
		return failCheck("home", "Gitmoot home is empty", true)
	}
	return okCheck("home", home, true)
}

func checkCanonicalSkill() pluginCheck {
	info, err := fs.Stat(skills.FS, "gitmoot/SKILL.md")
	if err != nil {
		return failCheck("canonical-skill", err.Error(), true)
	}
	if info.IsDir() {
		return failCheck("canonical-skill", "embedded SKILL.md is a directory", true)
	}
	return okCheck("canonical-skill", "embedded skills/gitmoot/SKILL.md is available", true)
}

func checkPackage(packagePath string) pluginCheck {
	info, err := os.Stat(packagePath)
	if err != nil {
		return failCheck("package", packagePath, true)
	}
	if !info.IsDir() {
		return failCheck("package", packagePath+" is not a directory", true)
	}
	return okCheck("package", packagePath, true)
}

func checkManifest(packagePath string, provider pluginpack.Provider) pluginCheck {
	manifest := pluginpack.ManifestPath(packagePath, provider)
	content, err := os.ReadFile(manifest)
	if err != nil {
		return failCheck("manifest", manifest, true)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		return failCheck("manifest", manifest+": "+err.Error(), true)
	}
	return okCheck("manifest", manifest, true)
}

func checkCopiedSkill(packagePath string) pluginCheck {
	path := filepath.Join(packagePath, "skills", pluginpack.PluginName, "SKILL.md")
	info, err := os.Stat(path)
	if err != nil {
		return failCheck("copied-skill", path, true)
	}
	if info.IsDir() {
		return failCheck("copied-skill", path+" is a directory", true)
	}
	return okCheck("copied-skill", path, true)
}

func checkMarketplacePath(home string, provider pluginpack.Provider) pluginCheck {
	return okCheck("marketplace-path", pluginpack.DefaultMarketplaceDir(home, provider), false)
}

func checkRuntimeCLI(provider pluginpack.Provider, explicitRuntime bool) pluginCheck {
	binary := string(provider)
	path, err := pluginLookPath(binary)
	if err != nil {
		required := explicitRuntime
		if required {
			return failCheck("runtime-cli", binary+" was not found on PATH", true)
		}
		return warnCheck("runtime-cli", binary+" was not found on PATH", false)
	}
	return okCheck("runtime-cli", path, true)
}

func checkClaudeAuthEnv() pluginCheck {
	auth := runtime.InspectClaudeAuthEnv(os.LookupEnv)
	detail := auth.MaskedDetail()
	if warning := auth.Warning(); warning != "" {
		detail += "; " + warning
	}
	if auth.Ready() && auth.Warning() == "" {
		return okCheck("runtime-auth-env", detail, false)
	}
	return warnCheck("runtime-auth-env", detail, false)
}

func checkValidationCommand(provider pluginpack.Provider, explicitRuntime bool) pluginCheck {
	switch provider {
	case pluginpack.ProviderClaude:
		if _, err := pluginLookPath("claude"); err != nil {
			return missingCheck("validation-command", "claude plugin validate requires claude on PATH", explicitRuntime)
		}
		return okCheck("validation-command", "claude plugin validate", true)
	case pluginpack.ProviderCodex:
		return warnCheck("validation-command", "codex plugin validation command is not exposed by the installed CLI", false)
	default:
		return failCheck("validation-command", "unknown runtime", true)
	}
}

func missingCheck(name, detail string, required bool) pluginCheck {
	if required {
		return failCheck(name, detail, true)
	}
	return warnCheck(name, detail, false)
}

func okCheck(name, detail string, required bool) pluginCheck {
	return pluginCheck{Name: name, Status: "ok", Detail: detail, Required: required}
}

func warnCheck(name, detail string, required bool) pluginCheck {
	return pluginCheck{Name: name, Status: "warn", Detail: detail, Required: required}
}

func failCheck(name, detail string, required bool) pluginCheck {
	return pluginCheck{Name: name, Status: "fail", Detail: detail, Required: required}
}

func runtimeChecksHealthy(checks []pluginCheck) bool {
	for _, check := range checks {
		if check.Status == "fail" {
			return false
		}
		if check.Name == "runtime-cli" && check.Status != "ok" {
			return false
		}
	}
	return true
}

func printPluginDoctor(w io.Writer, output pluginDoctorOutput) {
	writeLine(w, "home: %s", output.Home)
	for _, runtime := range output.Runtimes {
		status := "ok"
		if !runtime.Healthy {
			status = "fail"
		}
		writeLine(w, "%s: %s", runtime.Runtime, status)
		for _, check := range runtime.Checks {
			writeLine(w, "  %-18s %-5s %s", check.Name, check.Status, check.Detail)
		}
	}
}
