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
	"github.com/gitmoot/gitmoot/internal/keychain"
)

// Test-only seam for local httptest upstreams. Production configuration remains
// HTTPS-only even for loopback addresses.
var keyConfigureAllowLoopbackHTTP bool

type keychainPathStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type keychainListOutput struct {
	Keychain keychainPathStatus `json:"keychain"`
	Keys     []db.KeychainKey   `json:"keys"`
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
	default:
		fmt.Fprintf(stderr, "unknown key command %q\n", args[0])
		printKeyUsage(stderr)
		return 2
	}
}

func printKeyUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot key path [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key add <NAME> --mode proxied [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key list [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key show <NAME> [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key configure <NAME> --upstream <URL> --auth bearer|header:<HeaderName> [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot key rm <NAME> [--json] [--home DIR]")
}

func inspectKeychain(ctx context.Context, store *db.Store, home string) keychainPathStatus {
	path, err := keychain.ResolvePath(store, home)
	if err != nil {
		return keychainPathStatus{Status: "invalid", Detail: err.Error()}
	}
	status := keychainPathStatus{Path: path, Status: "ready"}
	if _, _, err := keychain.LoadValidated(ctx, store, home); err != nil {
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
	if err := keychain.ValidateName(name); err != nil {
		return fmt.Errorf("invalid key name %q: %w", name, err)
	}
	if keychain.ReservedName(name) {
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
	mode := fs.String("mode", "", "delivery mode: proxied")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *mode == "" {
		fmt.Fprintln(stderr, "key add requires one name and --mode proxied")
		return 2
	}
	if err := validateKeyName(name); err != nil {
		fmt.Fprintf(stderr, "key add: %v\n", err)
		return 2
	}
	if *mode != db.KeychainModeProxied {
		fmt.Fprintf(stderr, "key add: invalid mode %q; use proxied\n", *mode)
		return 2
	}
	var output db.KeychainKey
	var status keychainPathStatus
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if _, found, err := store.GetKeychainKey(ctx, name); err != nil {
			return err
		} else if found {
			return fmt.Errorf("key %s is already registered", name)
		}
		path, values, err := keychain.LoadValidated(ctx, store, *home)
		if err != nil {
			return err
		}
		if values[name] == "" {
			return fmt.Errorf("key %q is absent or empty in %s", name, path)
		}
		key, err := store.AddKeychainKey(ctx, name, *mode)
		output = key
		status = keychainPathStatus{Path: path, Status: "ready"}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "key add: %v\n", err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Keychain keychainPathStatus `json:"keychain"`
			Key      db.KeychainKey     `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "added key %s (%s)", output.Name, output.Mode)
	return 0
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
		output.Keys = keys
		output.Keychain = inspectKeychain(ctx, store, home)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "key list: %v\n", err)
		return 1
	}
	if output.Keys == nil {
		output.Keys = []db.KeychainKey{}
	}
	if jsonOut {
		return encodeKeyJSON(stdout, stderr, output)
	}
	for _, key := range output.Keys {
		writeLine(stdout, "%s\t%s\t%s\tupstream=%s\tauth=%s", key.Name, key.Mode, key.CreatedAt, firstNonEmpty(key.ProxyUpstream, "-"), keyProxyAuthLabel(key))
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
	// A LOOPBACK UPSTREAM IS A DIFFERENT RISK FROM A REMOTE PLAINTEXT ONE, which
	// the blanket HTTPS rule could not express. The real case is a local
	// credential broker such as `omp auth-gateway`: it holds the subscription
	// accounts and speaks each provider's login protocol, and it listens on
	// 127.0.0.1. The plaintext hop never leaves this host's loopback interface,
	// so demanding a certificate for 127.0.0.1 would protect against no attacker
	// that reaching loopback does not already imply.
	//
	// Opt-in per key and never inferred, and credgw still refuses any host that
	// is not genuinely loopback.
	allowLoopback := fs.Bool("allow-loopback", false, "permit a plaintext http:// upstream when its host is loopback (for a local credential broker)")
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
		AllowLoopbackHTTP: keyConfigureAllowLoopbackHTTP || *allowLoopback,
	})
	if err != nil {
		fmt.Fprintf(stderr, "key configure: %v\n", err)
		return 2
	}
	var output db.KeychainKey
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
		output, err = store.ConfigureKeychainProxy(ctx, name, policy.Upstream, string(policy.AuthKind), policy.Header)
		if err != nil {
			return err
		}
		status = inspectKeychain(ctx, store, *home)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "key configure: %v\n", err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Keychain keychainPathStatus `json:"keychain"`
			Key      db.KeychainKey     `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "configured key %s upstream=%s auth=%s", name, output.ProxyUpstream, keyProxyAuthLabel(output))
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
	var output db.KeychainKey
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
		output = key
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
			Key      db.KeychainKey     `json:"key"`
		}{status, output})
	}
	writeLine(stdout, "name: %s", output.Name)
	writeLine(stdout, "mode: %s", output.Mode)
	writeLine(stdout, "proxy_upstream: %s", firstNonEmpty(output.ProxyUpstream, "-"))
	writeLine(stdout, "proxy_auth: %s", keyProxyAuthLabel(output))
	writeLine(stdout, "created_at: %s", output.CreatedAt)
	writeLine(stdout, "keychain: %s (%s)", status.Path, status.Status)
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
	var status keychainPathStatus
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		removed, err = store.RemoveKeychainKey(context.Background(), name)
		status = inspectKeychain(context.Background(), store, *home)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "key rm: %v\n", err)
		return 1
	}
	if *jsonOut {
		return encodeKeyJSON(stdout, stderr, struct {
			Name             string             `json:"name"`
			Removed          bool               `json:"removed"`
			FileEntryRemains bool               `json:"file_entry_remains"`
			Keychain         keychainPathStatus `json:"keychain"`
		}{name, removed, true, status})
	}
	if !removed {
		writeLine(stdout, "key %s was not registered; keychain file unchanged", name)
		return 0
	}
	writeLine(stdout, "removed key %s metadata; entry in %s remains unchanged", name, status.Path)
	return 0
}
