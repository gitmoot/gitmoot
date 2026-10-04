package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/ompaddon"
)

const pluginOMP = "omp"

var ompLiveCheck = ompaddon.LiveCheck

// The OMP runtime has no plugin package or marketplace: Gitmoot installs one
// extension file, the inbox add-on, into OMP's user extensions directory.

type ompAddonLocation struct {
	paths         config.Paths
	extensionsDir string
	registryDir   string
}

func parseOMPPluginArgs(args []string, fs *flag.FlagSet, commandName string, stderr io.Writer) (ompAddonLocation, bool, bool) {
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		return ompAddonLocation{}, false, errors.Is(err, flag.ErrHelp)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept extra positional arguments\n", commandName)
		return ompAddonLocation{}, false, false
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return ompAddonLocation{}, false, false
	}
	// paths.Home is <user home>/.gitmoot; OMP's default agent dir sits beside it.
	extensionsDir, err := ompaddon.ExtensionsDir(filepath.Dir(paths.Home), os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)
		return ompAddonLocation{}, false, false
	}
	return ompAddonLocation{paths: paths, extensionsDir: extensionsDir, registryDir: paths.OMPRuntimeDir()}, true, false
}

func runPluginInstallOMP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin install omp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	loc, ok, help := parseOMPPluginArgs(args, fs, "plugin install omp", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}
	path, changed, err := ompaddon.Install(loc.extensionsDir, loc.registryDir)
	if err != nil {
		fmt.Fprintf(stderr, "plugin install omp: %v\n", err)
		return 1
	}
	writeLine(stdout, "extension: %s", path)
	writeLine(stdout, "registry: %s", loc.registryDir)
	writeLine(stdout, "version: %s", ompaddon.Version)
	if !changed {
		writeLine(stdout, "omp add-on already up to date")
		return 0
	}
	writeLine(stdout, "installed omp add-on")
	writeLine(stdout, "Running OMP sessions load it after /restart; /reload-plugins does not reload extensions.")
	return 0
}

func runPluginPathOMP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin path omp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	loc, ok, help := parseOMPPluginArgs(args, fs, "plugin path omp", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}
	writeLine(stdout, "%s", filepath.Join(loc.extensionsDir, ompaddon.FileName))
	return 0
}

func runPluginDoctorOMP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin doctor omp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "print doctor output as JSON")
	live := fs.Bool("live", false, "start omp in a throwaway session with a local model and check probe, idle delivery and deferral")
	loc, ok, help := parseOMPPluginArgs(args, fs, "plugin doctor omp", stderr)
	if help {
		return 0
	}
	if !ok {
		return 2
	}

	runtime := pluginDoctorRuntime{Runtime: pluginOMP, Path: filepath.Join(loc.extensionsDir, ompaddon.FileName)}
	runtime.Checks = append(runtime.Checks, checkHome(loc.paths.Home), checkOMPAddon(loc), checkOMPRegistryDir(loc.registryDir))
	ompPath, err := pluginLookPath(pluginOMP)
	if err != nil {
		runtime.Checks = append(runtime.Checks, failCheck("runtime-cli", "omp was not found on PATH", true))
	} else {
		runtime.Checks = append(runtime.Checks, okCheck("runtime-cli", ompPath, true))
	}
	if *live {
		if err != nil {
			runtime.Checks = append(runtime.Checks, failCheck("live", "omp was not found on PATH", true))
		} else {
			for _, step := range ompLiveCheck(context.Background(), ompPath) {
				check := okCheck("live-"+step.Name, step.Detail, true)
				if !step.OK {
					check = failCheck("live-"+step.Name, step.Detail, true)
				}
				runtime.Checks = append(runtime.Checks, check)
			}
		}
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
		fmt.Fprintln(stderr, "plugin doctor: omp runtime is unhealthy")
		return 1
	}
	return 0
}

func checkOMPAddon(loc ompAddonLocation) pluginCheck {
	inspection, err := ompaddon.Inspect(loc.extensionsDir, loc.registryDir)
	if err != nil {
		return failCheck("addon", err.Error(), true)
	}
	switch inspection.Status {
	case ompaddon.StatusInstalled:
		return okCheck("addon", fmt.Sprintf("installed, version %s", inspection.Version), true)
	case ompaddon.StatusMissing:
		return failCheck("addon", "missing; run gitmoot plugin install omp", true)
	default:
		installed := inspection.Version
		if installed == "" {
			installed = "unknown"
		}
		detail := fmt.Sprintf("outdated: installed version %s, this gitmoot installs %s", installed, inspection.ExpectedVersion)
		if inspection.RegistryDir != inspection.ExpectedRegistryDir {
			detail += fmt.Sprintf("; installed for registry %q, expected %q", inspection.RegistryDir, inspection.ExpectedRegistryDir)
		}
		return failCheck("addon", detail+"; run gitmoot plugin install omp", true)
	}
}

func checkOMPRegistryDir(dir string) pluginCheck {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return okCheck("registry-dir", dir+" (created by the first OMP session)", false)
	case err != nil:
		return warnCheck("registry-dir", err.Error(), false)
	case !info.IsDir():
		return failCheck("registry-dir", dir+" is not a directory", true)
	case info.Mode().Perm() != 0o700:
		return warnCheck("registry-dir", fmt.Sprintf("%s has mode %v; the add-on resets it to 0700", dir, info.Mode().Perm()), false)
	}
	return okCheck("registry-dir", dir, false)
}
