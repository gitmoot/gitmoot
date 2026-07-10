package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
			Name:             o.Name,
			DefaultModel:     o.DefaultModel,
			DefaultModelSet:  o.DefaultModelSet,
			DefaultEffort:    o.DefaultEffort,
			DefaultEffortSet: o.DefaultEffortSet,
			Models:           o.Models,
			ModelsSet:        o.ModelsSet,
			Capabilities:     o.Capabilities,
			CapabilitiesSet:  o.CapabilitiesSet,
			UsageSource:      o.UsageSource,
			UsageSourceSet:   o.UsageSourceSet,
		})
	}
	return out
}

// resolveRuntimeRegistryResilient builds the effective runtime metadata registry
// the same way resolveRuntimeRegistry does, but is PER-SECTION resilient: a single
// malformed [runtimes.<name>] section (an unknown-runtime typo like
// [runtimes.codxe], or an invalid capability) is SKIPPED with a logged warning
// naming the offending section instead of failing the whole config. Every VALID
// [runtimes.<name>] section's overrides still take effect. This is the DELIVERY
// path: it must never drop otherwise-valid default_model/default_effort overrides
// just because one unrelated section has a typo. A missing config file (fresh
// box), an empty home,
// or a file-level parse error yields the built-in registry unchanged (fail-safe:
// nothing is forced). Overrides are applied one section at a time so a rejected
// section cannot poison the accumulated valid ones.
func resolveRuntimeRegistryResilient(paths config.Paths) runtime.Registry {
	registry := runtime.BuiltinRuntimeRegistry()
	overrides, err := config.LoadRuntimeOverrides(paths)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("runtime registry: ignoring config overrides for delivery defaults resolution: %v", err)
		}
		return registry
	}
	for _, o := range overrides {
		patched, err := registry.ApplyOverrides(runtimeMetadataOverrides([]config.RuntimeOverride{o}))
		if err != nil {
			log.Printf("runtime registry: skipping invalid [runtimes.%s] section for delivery defaults resolution: %v", o.Name, err)
			continue
		}
		registry = patched
	}
	return registry
}

// runtimeDefaultModelResolver returns a HOME-AWARE resolver for a runtime's
// configured registry default_model (#652), suitable for wiring into
// workflow.Engine.RuntimeDefaultModel / workflow.Mailbox.RuntimeDefaultModel so a
// delivered job with no agent --model and no job --model falls back to it. It reads
// the built-in registry overlaid with any [runtimes.<name>] config overrides for
// `home` (which may be an already-resolved <home>/.gitmoot root OR a raw --home;
// resolveConfigFile handles both), then returns the resolved DefaultModel for the
// named runtime. It is FAIL-SAFE and PER-SECTION resilient: a missing config, an
// empty home, or an unknown runtime yields ""; a single malformed [runtimes.<name>]
// section is skipped with a logged warning while OTHER valid sections'
// default_model overrides still resolve (so one typo can no longer silently drop
// every override at delivery). It re-reads config on each call, so a warm-reloaded
// (SIGHUP) default_model edit takes effect without a full restart.
func runtimeDefaultModelResolver(home string) func(string) string {
	return func(runtimeName string) string {
		registry := resolveRuntimeRegistryResilient(config.Paths{ConfigFile: resolveConfigFile(home)})
		meta, ok := registry.Metadata(strings.TrimSpace(runtimeName))
		if !ok {
			return ""
		}
		return strings.TrimSpace(meta.DefaultModel)
	}
}

// runtimeDefaultEffortResolver returns a HOME-AWARE resolver for a runtime's
// configured registry default_effort. It mirrors runtimeDefaultModelResolver:
// config is re-read on every call, malformed sibling sections are skipped, and
// missing config or an unknown runtime resolves to empty (no effort override).
func runtimeDefaultEffortResolver(home string) func(string) string {
	return func(runtimeName string) string {
		registry := resolveRuntimeRegistryResilient(config.Paths{ConfigFile: resolveConfigFile(home)})
		meta, ok := registry.Metadata(strings.TrimSpace(runtimeName))
		if !ok {
			return ""
		}
		return strings.TrimSpace(meta.DefaultEffort)
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
	fmt.Fprintln(w, "model/effort, known models, and where token usage is read from. Values come from the")
	fmt.Fprintln(w, "compiled built-in defaults, overlaid with any [runtimes.<name>] config overrides.")
}

// runtimeListEntry is the JSON shape for `gitmoot runtime list --json`.
type runtimeListEntry struct {
	Name          string   `json:"name"`
	Dispatchable  bool     `json:"dispatchable"`
	Capabilities  []string `json:"capabilities"`
	DefaultModel  string   `json:"default_model"`
	DefaultEffort string   `json:"default_effort"`
	Models        []string `json:"models"`
	UsageSource   string   `json:"usage_source"`
	Description   string   `json:"description"`
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
				Name:          m.Name,
				Dispatchable:  m.Dispatchable,
				Capabilities:  m.Capabilities,
				DefaultModel:  m.DefaultModel,
				DefaultEffort: m.DefaultEffort,
				Models:        m.Models,
				UsageSource:   m.UsageSource,
				Description:   m.Description,
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
	fmt.Fprintln(tw, "RUNTIME\tCAPABILITIES\tDEFAULT MODEL\tDEFAULT EFFORT\tKNOWN MODELS\tUSAGE SOURCE")
	for _, m := range metas {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Name,
			strings.Join(m.Capabilities, ","),
			orDash(m.DefaultModel),
			orDash(m.DefaultEffort),
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
