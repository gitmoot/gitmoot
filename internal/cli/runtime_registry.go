package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// resolveRuntimeRegistry builds the effective runtime metadata registry: the
// compiled built-in defaults (which reproduce today's behavior exactly) overlaid
// with any [runtimes.<name>] operator overrides from the config file. A missing
// config file (fresh box) or an absent [runtimes.*] section yields the built-in
// registry unchanged, so the default path is byte-identical.
func resolveRuntimeRegistry(paths config.Paths) (runtime.Registry, error) {
	registry := runtime.BuiltinRuntimeRegistry()
	overrides, err := config.LoadRuntimeOverrides(paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return registry, nil
		}
		return runtime.Registry{}, err
	}
	if len(overrides) == 0 {
		return registry, nil
	}
	return registry.ApplyOverrides(runtimeMetadataOverrides(overrides))
}

// runtimeMetadataOverrides translates the neutral config.RuntimeOverride data
// into the runtime package's MetadataOverride patches. It exists so config does
// not import runtime and runtime does not import config — the CLI layer, which
// imports both, is the single translation seam.
func runtimeMetadataOverrides(overrides []config.RuntimeOverride) []runtime.MetadataOverride {
	out := make([]runtime.MetadataOverride, 0, len(overrides))
	for _, o := range overrides {
		out = append(out, runtime.MetadataOverride{
			Name:            o.Name,
			DefaultModel:    o.DefaultModel,
			DefaultModelSet: o.DefaultModelSet,
			Models:          o.Models,
			ModelsSet:       o.ModelsSet,
			Capabilities:    o.Capabilities,
			CapabilitiesSet: o.CapabilitiesSet,
			UsageSource:     o.UsageSource,
			UsageSourceSet:  o.UsageSourceSet,
		})
	}
	return out
}

// runtimeDefaultModelResolver returns a HOME-AWARE resolver for a runtime's
// configured registry default_model (#652), suitable for wiring into
// workflow.Engine.RuntimeDefaultModel / workflow.Mailbox.RuntimeDefaultModel so a
// delivered job with no agent --model and no job --model falls back to it. It reads
// the built-in registry overlaid with any [runtimes.<name>] config overrides for
// `home` (which may be an already-resolved <home>/.gitmoot root OR a raw --home;
// resolveConfigFile handles both), then returns the resolved DefaultModel for the
// named runtime. It is FAIL-OPEN: a resolution error, a missing config, an empty
// home, or an unknown runtime all yield "", so delivery forces nothing and is
// byte-identical to before #652. It re-reads config on each call, so a warm-reloaded
// (SIGHUP) default_model edit takes effect without a full restart.
func runtimeDefaultModelResolver(home string) func(string) string {
	return func(runtimeName string) string {
		registry, err := resolveRuntimeRegistry(config.Paths{ConfigFile: resolveConfigFile(home)})
		if err != nil {
			return ""
		}
		meta, ok := registry.Metadata(strings.TrimSpace(runtimeName))
		if !ok {
			return ""
		}
		return strings.TrimSpace(meta.DefaultModel)
	}
}

func runRuntime(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printRuntimeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runRuntimeList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown runtime command %q\n\n", args[0])
		printRuntimeUsage(stderr)
		return 2
	}
}

func printRuntimeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot runtime list [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Shows the resolved metadata for each built-in runtime: capabilities, default")
	fmt.Fprintln(w, "and known models, and where token usage is read from. Values come from the")
	fmt.Fprintln(w, "compiled built-in defaults, overlaid with any [runtimes.<name>] config overrides.")
}

// runtimeListEntry is the JSON shape for `gitmoot runtime list --json`.
type runtimeListEntry struct {
	Name         string   `json:"name"`
	Dispatchable bool     `json:"dispatchable"`
	Capabilities []string `json:"capabilities"`
	DefaultModel string   `json:"default_model"`
	Models       []string `json:"models"`
	UsageSource  string   `json:"usage_source"`
	Description  string   `json:"description"`
}

func runRuntimeList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runtime list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the runtimes as a JSON array instead of the text table")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "runtime list does not accept positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "runtime list: %v\n", err)
		return 1
	}
	registry, err := resolveRuntimeRegistry(paths)
	if err != nil {
		fmt.Fprintf(stderr, "runtime list: %v\n", err)
		return 1
	}
	metas := registry.All()
	if *jsonOutput {
		entries := make([]runtimeListEntry, 0, len(metas))
		for _, m := range metas {
			entries = append(entries, runtimeListEntry{
				Name:         m.Name,
				Dispatchable: m.Dispatchable,
				Capabilities: m.Capabilities,
				DefaultModel: m.DefaultModel,
				Models:       m.Models,
				UsageSource:  m.UsageSource,
				Description:  m.Description,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(stderr, "runtime list: %v\n", err)
			return 1
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tCAPABILITIES\tDEFAULT MODEL\tKNOWN MODELS\tUSAGE SOURCE")
	for _, m := range metas {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			m.Name,
			strings.Join(m.Capabilities, ","),
			orDash(m.DefaultModel),
			orDash(strings.Join(m.Models, ",")),
			orDash(m.UsageSource),
		)
	}
	_ = tw.Flush()
	return 0
}

// orDash renders an empty string as "-" so a table cell is never blank.
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
