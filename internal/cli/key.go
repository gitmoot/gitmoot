package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

// Test-only seam for local httptest upstreams. Production configuration remains
// HTTPS-only even for loopback addresses.
var keyConfigureAllowLoopbackHTTP bool

type keychainPathStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type keychainKeyOutput struct {
	db.KeychainKey
	Grants []db.KeychainGrant `json:"grants"`
}

type keychainListOutput struct {
	Keychain keychainPathStatus  `json:"keychain"`
	Keys     []keychainKeyOutput `json:"keys"`
}

func runKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stdout)
		return 0
	}
	switch args[0] {
	case "path":
		return runKeyPath(args[1:], stdout, stderr)
	case "add":
		return runKeyAdd(args[1:], stdout, stderr)
	case "list":
		return runKeyList(args[1:], stdout, stderr)
	case "show":
		return runKeyShow(args[1:], stdout, stderr)
	case "configure":
		return runKeyConfigure(args[1:], stdout, stderr)
	case "rm":
		return runKeyRemove(args[1:], stdout, stderr)
	case "grant":
		return runKeyGrant(args[1:], stdout, stderr)
	case "revoke":
		return runKeyRevoke(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown key command %q\n", args[0])
		printKeyUsage(stderr)
		return 2
	}
}

func printKeyUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot key path [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key add <NAME> --mode injected|proxied [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key list [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key show <NAME> [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key configure <NAME> --upstream <URL> --auth bearer|header:<HeaderName> [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key rm <NAME> [--force] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key grant <NAME> --pipeline <PIPELINE> [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key revoke <NAME> --pipeline <PIPELINE> [--json] [--home DIR]")
}

func inspectKeychain(ctx context.Context, store *db.Store, home string) keychainPathStatus {
	path, err := resolveKeychainPath(store, home)
	if err != nil {
		return keychainPathStatus{Status: "invalid", Detail: err.Error()}
	}
	status := keychainPathStatus{Path: path, Status: "ready"}
	if _, _, err := loadValidatedKeychainFile(ctx, store, home); err != nil {
		status.Status = "invalid"
		status.Detail = err.Error()
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			status.Status = "missing"
			status.Detail = ""
		}
	}
	return status
}

func encodeKeyJSON(stdout, stderr io.Writer, value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintf(stderr, "key: %v\n", err)
		return 1
	}
	return 0
}

func parseKeyNoPositionals(command string, args []string, stderr io.Writer) (home string, jsonOut bool, code int) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&home, "home", "", "home directory to use instead of the current user's home")
	fs.BoolVar(&jsonOut, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", false, 0
		}
		return "", false, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept positional arguments\n", command)
		return "", false, 2
	}
	return home, jsonOut, -1
}

func runKeyPath(args []string, stdout, stderr io.Writer) int {
	home, jsonOut, code := parseKeyNoPositionals("key path", args, stderr)
	if code >= 0 {
		return code
	}
	var status keychainPathStatus
	if err := withStore(home, func(store *db.Store) error {
		status = inspectKeychain(context.Background(), store, home)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "key path: %v\n", err)
		return 1
	}
	if jsonOut {
		return encodeKeyJSON(stdout, stderr, status)
	}
	writeLine(stdout, "%s\t%s", status.Path, status.Status)
	if status.Detail != "" {
		writeLine(stdout, "  %s", status.Detail)
	}
	return 0
}

func validateKeyName(name string) error {
	if err := pipeline.ValidateEnvName(name); err != nil {
		return fmt.Errorf("invalid key name %q: %w", name, err)
	}
	if pipeline.ReservedEnvName(name) {
		return fmt.Errorf("key name %q uses reserved GITMOOT_* namespace", name)
	}
	return nil
}

func runKeyAdd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "key add requires a name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("key add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print JSON")
	mode := fs.String("mode", "", "delivery mode: injected or proxied")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *mode == "" {
		fmt.Fprintln(stderr, "key add requires one name and --mode injected|proxied")
		return 2
	}
	if err := validateKeyName(name); err != nil {
		fmt.Fprintf(stderr, "key add: %v\n", err)
		return 2
	}
	if *mode != db.KeychainModeInjected && *mode != db.KeychainModeProxied {
		fmt.Fprintf(stderr, "key add: invalid mode %q; use injected or proxied\n", *mode)
		return 2
	}
	var output keychainKeyOutput
	var status keychainPathStatus
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if _, found, err := store.GetKeychainKey(ctx, name); err != nil {
			return err
		} else if found {
			return fmt.Errorf("key %s is already registered", name)
		}
		path, values, err := loadValidatedKeychainFile(ctx, store, *home)
		if err != nil {
			return err
		}
		if values[name] == "" {
			return fmt.Errorf("key %q is absent or empty in %s", name, path)
		}
		key, err := store.AddKeychainKey(ctx, name, *mode)
		output = keychainKeyOutput{KeychainKey: key, Grants: []db.KeychainGrant{}}
		status = keychainPathStatus{Path: path, Status: "ready"}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "key add: %v\n", err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Keychain keychainPathStatus `json:"keychain"`
			Key      keychainKeyOutput  `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "added key %s (%s)", output.Name, output.Mode)
	return 0
}

func keyOutputs(ctx context.Context, store *db.Store, keys []db.KeychainKey) ([]keychainKeyOutput, error) {
	out := make([]keychainKeyOutput, 0, len(keys))
	for _, key := range keys {
		grants, err := store.ListKeychainGrants(ctx, key.Name)
		if err != nil {
			return nil, err
		}
		if grants == nil {
			grants = []db.KeychainGrant{}
		}
		out = append(out, keychainKeyOutput{KeychainKey: key, Grants: grants})
	}
	return out, nil
}

func runKeyList(args []string, stdout, stderr io.Writer) int {
	home, jsonOut, code := parseKeyNoPositionals("key list", args, stderr)
	if code >= 0 {
		return code
	}
	var output keychainListOutput
	if err := withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		keys, err := store.ListKeychainKeys(ctx)
		if err != nil {
			return err
		}
		output.Keys, err = keyOutputs(ctx, store, keys)
		output.Keychain = inspectKeychain(ctx, store, home)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "key list: %v\n", err)
		return 1
	}
	if output.Keys == nil {
		output.Keys = []keychainKeyOutput{}
	}
	if jsonOut {
		return encodeKeyJSON(stdout, stderr, output)
	}
	for _, key := range output.Keys {
		writeLine(stdout, "%s\t%s\t%s\tgrants=%d\tupstream=%s\tauth=%s", key.Name, key.Mode, key.CreatedAt, len(key.Grants), firstNonEmpty(key.ProxyUpstream, "-"), keyProxyAuthLabel(key.KeychainKey))
	}
	return 0
}

func runKeyShow(args []string, stdout, stderr io.Writer) int {
	return runKeyNamedRead("show", args, stdout, stderr)
}

func runKeyConfigure(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "key configure requires a name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("key configure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print JSON")
	upstream := fs.String("upstream", "", "fixed HTTPS upstream origin and base path")
	auth := fs.String("auth", "", "credential placement: bearer or header:<HeaderName>")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*upstream) == "" || strings.TrimSpace(*auth) == "" {
		fmt.Fprintln(stderr, "key configure requires one name, --upstream <url>, and --auth bearer|header:<HeaderName>")
		return 2
	}
	authKind, header, err := parseKeyProxyAuth(*auth)
	if err != nil {
		fmt.Fprintf(stderr, "key configure: %v\n", err)
		return 2
	}
	policy, _, err := credgw.ValidateProxyPolicy(credgw.ProxyPolicy{
		Upstream: strings.TrimSpace(*upstream), AuthKind: authKind, Header: header,
		AllowLoopbackHTTP: keyConfigureAllowLoopbackHTTP,
	})
	if err != nil {
		fmt.Fprintf(stderr, "key configure: %v\n", err)
		return 2
	}
	var output keychainKeyOutput
	var status keychainPathStatus
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		key, found, err := store.GetKeychainKey(ctx, name)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("key %s is not registered", name)
		}
		if key.Mode != db.KeychainModeProxied {
			return fmt.Errorf("key %s uses %s mode; key configure requires proxied mode", name, key.Mode)
		}
		configured, err := store.ConfigureKeychainProxy(ctx, name, policy.Upstream, string(policy.AuthKind), policy.Header)
		if err != nil {
			return err
		}
		rows, err := keyOutputs(ctx, store, []db.KeychainKey{configured})
		if err != nil {
			return err
		}
		output = rows[0]
		status = inspectKeychain(ctx, store, *home)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "key configure: %v\n", err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Keychain keychainPathStatus `json:"keychain"`
			Key      keychainKeyOutput  `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "configured key %s upstream=%s auth=%s", name, output.ProxyUpstream, keyProxyAuthLabel(output.KeychainKey))
	return 0
}

func parseKeyProxyAuth(raw string) (credgw.ProxyAuthKind, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == string(credgw.ProxyAuthBearer) {
		return credgw.ProxyAuthBearer, "", nil
	}
	if header, ok := strings.CutPrefix(raw, "header:"); ok && strings.TrimSpace(header) != "" {
		return credgw.ProxyAuthHeader, strings.TrimSpace(header), nil
	}
	return "", "", fmt.Errorf("invalid --auth %q; use bearer or header:<HeaderName>", raw)
}

func keyProxyAuthLabel(key db.KeychainKey) string {
	if key.ProxyAuthKind == db.KeychainProxyAuthHeader {
		return "header:" + key.ProxyHeader
	}
	if key.ProxyAuthKind == db.KeychainProxyAuthBearer {
		return db.KeychainProxyAuthBearer
	}
	return "-"
}

func runKeyNamedRead(verb string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintf(stderr, "key %s requires a name\n", verb)
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("key "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "key %s accepts exactly one name\n", verb)
		return 2
	}
	var output keychainKeyOutput
	var status keychainPathStatus
	found := false
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		key, ok, err := store.GetKeychainKey(ctx, name)
		if err != nil || !ok {
			found = ok
			return err
		}
		found = true
		rows, err := keyOutputs(ctx, store, []db.KeychainKey{key})
		if err != nil {
			return err
		}
		output = rows[0]
		status = inspectKeychain(ctx, store, *home)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "key %s: %v\n", verb, err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "key %s: key %s not found\n", verb, name)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Keychain keychainPathStatus `json:"keychain"`
			Key      keychainKeyOutput  `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "name: %s", output.Name)
	writeLine(stdout, "mode: %s", output.Mode)
	writeLine(stdout, "proxy_upstream: %s", firstNonEmpty(output.ProxyUpstream, "-"))
	writeLine(stdout, "proxy_auth: %s", keyProxyAuthLabel(output.KeychainKey))
	writeLine(stdout, "created_at: %s", output.CreatedAt)
	writeLine(stdout, "keychain: %s (%s)", status.Path, status.Status)
	writeLine(stdout, "grants:")
	for _, grant := range output.Grants {
		writeLine(stdout, "  %s:%s", grant.ConsumerKind, grant.ConsumerID)
	}
	return 0
}

func runKeyRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "key rm requires a name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("key rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print JSON")
	force := fs.Bool("force", false, "remove metadata and grants even when grants exist")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "key rm accepts exactly one name")
		return 2
	}
	var removed bool
	var grantsRemoved int
	var status keychainPathStatus
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		removed, grantsRemoved, err = store.RemoveKeychainKey(context.Background(), name, *force)
		status = inspectKeychain(context.Background(), store, *home)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrKeychainKeyHasGrants) {
			fmt.Fprintf(stderr, "key rm: key %s has %d grant(s); revoke them or use --force\n", name, grantsRemoved)
		} else {
			fmt.Fprintf(stderr, "key rm: %v\n", err)
		}
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Name             string             `json:"name"`
			Removed          bool               `json:"removed"`
			GrantsRemoved    int                `json:"grants_removed"`
			FileEntryRemains bool               `json:"file_entry_remains"`
			Keychain         keychainPathStatus `json:"keychain"`
		}{name, removed, grantsRemoved, true, status})
	}
	if !removed {
		writeLine(stdout, "key %s was not registered; keychain file unchanged", name)
		return 0
	}
	writeLine(stdout, "removed key %s metadata and %d grant(s); entry in %s remains unchanged", name, grantsRemoved, status.Path)
	return 0
}

func runKeyGrant(args []string, stdout, stderr io.Writer) int {
	return runKeyGrantMutation(true, args, stdout, stderr)
}

func runKeyRevoke(args []string, stdout, stderr io.Writer) int {
	return runKeyGrantMutation(false, args, stdout, stderr)
}

func runKeyGrantMutation(grant bool, args []string, stdout, stderr io.Writer) int {
	verb := "grant"
	if !grant {
		verb = "revoke"
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printKeyUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintf(stderr, "key %s requires a name\n", verb)
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("key "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print JSON")
	pipelineName := fs.String("pipeline", "", "pipeline consumer")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*pipelineName) == "" {
		fmt.Fprintf(stderr, "key %s requires one name and --pipeline <pipeline>\n", verb)
		return 2
	}
	changed := false
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if grant {
			if _, found, err := store.GetPipeline(ctx, *pipelineName); err != nil {
				return err
			} else if !found {
				return fmt.Errorf("pipeline %s not found", *pipelineName)
			}
			key, found, err := store.GetKeychainKey(ctx, name)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("key %s is not registered", name)
			}
			if key.Mode == db.KeychainModeProxied && !key.ProxyConfigured() {
				return fmt.Errorf("key %s uses proxied mode but is not configured; run %s", name, keyConfigureCommand(name))
			}
			path, values, err := loadValidatedKeychainFile(ctx, store, *home)
			if err != nil {
				return err
			}
			if values[name] == "" {
				return fmt.Errorf("key %q is absent or empty in %s", name, path)
			}
			changed, err = store.GrantKeychainKey(ctx, db.KeychainConsumerPipeline, *pipelineName, name)
			return err
		}
		var err error
		changed, err = store.RevokeKeychainKey(ctx, db.KeychainConsumerPipeline, *pipelineName, name)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "key %s: %v\n", verb, err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Name     string `json:"name"`
			Pipeline string `json:"pipeline"`
			Changed  bool   `json:"changed"`
		}{name, strings.TrimSpace(*pipelineName), changed})
	}
	past := "granted"
	if !grant {
		past = "revoked"
	}
	if !changed {
		writeLine(stdout, "key %s already %s for pipeline %s", name, past, *pipelineName)
		return 0
	}
	writeLine(stdout, "%s key %s for pipeline %s", past, name, *pipelineName)
	return 0
}

func keyConfigureCommand(name string) string {
	return fmt.Sprintf("gitmoot key configure %s --upstream <url> --auth bearer|header:<HeaderName>", name)
}
