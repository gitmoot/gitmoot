package cli

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/hub"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	hubDefaultMachineDays = 365
	hubMaxMachineDays     = 3 * 365
	hubMaxRequestBody     = 64 << 10
)

func runHub(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || helpRequested(args[:1]) {
		printHubUsage(stdout)
		return 0
	}
	if args[0] != "machine" {
		fmt.Fprintf(stderr, "unknown hub command %q\n\n", args[0])
		printHubUsage(stderr)
		return 2
	}
	if len(args) < 2 || helpRequested(args[1:2]) {
		printHubUsage(stdout)
		return 0
	}
	if len(args) > 2 && helpRequested(args[2:3]) {
		printHubUsage(stdout)
		return 0
	}
	switch args[1] {
	case "add":
		return runHubMachineAdd(args[2:], stdout, stderr)
	case "ls", "list":
		return runHubMachineList(args[2:], stdout, stderr)
	case "revoke":
		return runHubMachineRevoke(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown hub machine command %q\n\n", args[1])
		printHubUsage(stderr)
		return 2
	}
}

func printHubUsage(w io.Writer) {
	fmt.Fprintln(w, "The hub API: gitmoot clients on other machines, over Tailscale with client certificates (#2710).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot hub machine add NAME --role ROLE [--role ROLE]... --out DIR [--days 365] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot hub machine ls [--all] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot hub machine revoke NAME [--home DIR]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "add issues a client certificate from the hub's authority and writes ca.pem,")
	fmt.Fprintln(w, "NAME.pem and NAME.key (mode 600) into DIR, a new or empty directory. Copy them")
	fmt.Fprintln(w, "to the machine; the private key exists nowhere else. The machine may act as")
	fmt.Fprintln(w, "the listed org roles only. revoke stops the certificate on its next request.")
	fmt.Fprintln(w, "The daemon serves the hub when [hub] listen is set.")
}

type hubMachineJSON struct {
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	Roles       []string `json:"roles"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at"`
	RevokedAt   string   `json:"revoked_at,omitempty"`
	Files       []string `json:"files,omitempty"`
}

func newHubMachineJSON(machine db.HubMachine) hubMachineJSON {
	out := hubMachineJSON{Name: machine.Name, Fingerprint: machine.Fingerprint, Roles: machine.Roles,
		CreatedAt: machine.CreatedAt.UTC().Format(time.RFC3339), ExpiresAt: machine.ExpiresAt.UTC().Format(time.RFC3339)}
	if !machine.RevokedAt.IsZero() {
		out.RevokedAt = machine.RevokedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// validHubMachineName keeps a machine name usable as a file name and a
// certificate subject.
func validHubMachineName(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[0] == '.' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

func runHubMachineAdd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || helpRequested(args) {
		printHubUsage(stdout)
		return 0
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	fs := flag.NewFlagSet("hub machine add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	out := fs.String("out", "", "directory for ca.pem, NAME.pem and NAME.key")
	days := fs.Int("days", hubDefaultMachineDays, "certificate lifetime in days")
	jsonOutput := fs.Bool("json", false, "JSON output")
	var roles repeatedFlag
	fs.Var(&roles, "role", "org role the machine may act as (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || !validHubMachineName(name) {
		fmt.Fprintln(stderr, "hub machine add: NAME must be 1-63 of a-z 0-9 - . _, not starting with - or .")
		return 2
	}
	if len(roles) == 0 || strings.TrimSpace(*out) == "" || *days < 1 || *days > hubMaxMachineDays {
		fmt.Fprintf(stderr, "hub machine add: needs at least one --role, --out DIR, and --days 1-%d\n", hubMaxMachineDays)
		return 2
	}
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		org, err := config.LoadOrg(paths)
		if err != nil {
			return err
		}
		normalized := make([]string, 0, len(roles))
		for _, role := range roles {
			role = strings.ToLower(strings.TrimSpace(role))
			if _, ok := org.Role(role); !ok {
				return fmt.Errorf("unknown role %q", role)
			}
			normalized = append(normalized, role)
		}
		outDir, err := prepareHubOutDir(*out)
		if err != nil {
			return err
		}
		authority, err := hub.LoadOrCreateAuthority(hub.Dir(paths.Home))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		identity := &url.URL{Scheme: "spiffe", Host: "gitmoot", Path: "/machine/" + name}
		leaf, err := authority.IssueClient(name, identity, now.Add(time.Duration(*days)*24*time.Hour))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), orgSeatExternalTimeout)
		defer cancel()
		machine, err := store.AddHubMachine(ctx, db.HubMachine{Name: name, Fingerprint: hex.EncodeToString(leaf.Fingerprint[:]),
			Roles: normalized, CreatedAt: now, ExpiresAt: leaf.NotAfter})
		if err != nil {
			return err
		}
		files := []struct {
			name string
			data []byte
			mode os.FileMode
		}{
			{"ca.pem", authority.CertificatePEM(), 0o644},
			{name + ".pem", leaf.CertificatePEM, 0o644},
			{name + ".key", leaf.PrivateKeyPEM, 0o600},
		}
		written := make([]string, 0, len(files))
		for _, file := range files {
			path := filepath.Join(outDir, file.name)
			if err := writeNewFile(path, file.data, file.mode); err != nil {
				// The certificate is unusable without its files; revoke it so no
				// half-delivered machine stays registered.
				_, _ = store.RevokeHubMachine(ctx, name, time.Now().UTC())
				return fmt.Errorf("write %s (machine %s revoked): %w", path, name, err)
			}
			written = append(written, path)
		}
		if *jsonOutput {
			result := newHubMachineJSON(machine)
			result.Files = written
			return writeJSON(stdout, result)
		}
		fmt.Fprintf(stdout, "machine %s may act as %s until %s\n", machine.Name, strings.Join(machine.Roles, ", "), machine.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintf(stdout, "fingerprint %s\n", machine.Fingerprint)
		for _, path := range written {
			fmt.Fprintf(stdout, "wrote %s\n", path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "hub machine add:", err)
		return 1
	}
	return 0
}

// prepareHubOutDir returns an absolute directory that is new or empty, so a
// key is never written next to, or over, unrelated files.
func prepareHubOutDir(raw string) (string, error) {
	dir, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("--out %s is not empty", dir)
	}
	return dir, nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func runHubMachineList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub machine ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	all := fs.Bool("all", false, "include revoked machines")
	jsonOutput := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected hub machine ls argument")
		return 2
	}
	err := withStoreAndPaths(*home, func(_ config.Paths, store *db.Store) error {
		machines, err := store.ListHubMachines(context.Background(), *all)
		if err != nil {
			return err
		}
		if *jsonOutput {
			out := make([]hubMachineJSON, len(machines))
			for index, machine := range machines {
				out[index] = newHubMachineJSON(machine)
			}
			return writeJSON(stdout, out)
		}
		if len(machines) == 0 {
			fmt.Fprintln(stdout, "No machines.")
			return nil
		}
		table := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(table, "NAME\tROLES\tEXPIRES\tREVOKED\tFINGERPRINT")
		for _, machine := range machines {
			revoked := "-"
			if !machine.RevokedAt.IsZero() {
				revoked = machine.RevokedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", machine.Name, strings.Join(machine.Roles, ","),
				machine.ExpiresAt.UTC().Format(time.RFC3339), revoked, machine.Fingerprint[:16])
		}
		return table.Flush()
	})
	if err != nil {
		fmt.Fprintln(stderr, "hub machine ls:", err)
		return 1
	}
	return 0
}

func runHubMachineRevoke(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || helpRequested(args) {
		printHubUsage(stdout)
		return 0
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	fs := flag.NewFlagSet("hub machine revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return 2
	}
	err := withStoreAndPaths(*home, func(_ config.Paths, store *db.Store) error {
		machine, err := store.RevokeHubMachine(context.Background(), name, time.Now().UTC())
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no unrevoked machine named %q", name)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked machine %s (fingerprint %s)\n", machine.Name, machine.Fingerprint)
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "hub machine revoke:", err)
		return 1
	}
	return 0
}

// hubAPI serves the hub's endpoints for authenticated callers. Each endpoint
// calls the function its CLI command calls, so remote and local results
// match, and loads the org config per request, so a role removed from the
// org stops working remotely at once.
//
//	GET  /v1/whoami
//	GET  /v1/messages?before=ID&limit=N          inbox
//	GET  /v1/messages/{id}?thread=1&before&limit show
//	POST /v1/messages {"to","body","campaign"}   send
//	POST /v1/messages/{id}/reply {"body"}        reply
func hubAPI(paths config.Paths, store *db.Store) http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, serve func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			caller, ok := hub.CallerFrom(r.Context())
			if !ok {
				hub.WriteError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			org, err := config.LoadOrg(paths)
			if err != nil {
				hub.WriteError(w, http.StatusInternalServerError, "org config unavailable")
				return
			}
			if _, ok := org.Role(caller.Role); !ok {
				hub.WriteError(w, http.StatusForbidden, fmt.Sprintf("role %q is not in the org", caller.Role))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, hubMaxRequestBody)
			status, value, err := serve(r.Context(), caller, org, r)
			if err != nil {
				hub.WriteError(w, status, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = writeJSON(w, value)
		})
	}
	handle("GET /v1/whoami", func(_ context.Context, caller hub.Caller, _ config.OrgConfig, _ *http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"machine": caller.Machine.Name, "role": caller.Role, "roles": caller.Machine.Roles}, nil
	})
	handle("GET /v1/messages", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		before, limit, err := hubPage(r)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		messages, err := store.ListMessages(ctx, caller.Role, before, limit)
		if err != nil {
			return http.StatusInternalServerError, nil, errors.New("inbox unavailable")
		}
		return http.StatusOK, newMessagesJSON(messages), nil
	})
	handle("GET /v1/messages/{id}", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		id, err := hubMessageID(r)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		if r.URL.Query().Get("thread") != "" {
			before, limit, err := hubPage(r)
			if err != nil {
				return http.StatusBadRequest, nil, err
			}
			messages, err := store.ListMessageThread(ctx, id, caller.Role, before, limit)
			if err != nil {
				return http.StatusInternalServerError, nil, errors.New("thread unavailable")
			}
			return http.StatusOK, newMessagesJSON(messages), nil
		}
		message, err := store.GetMessage(ctx, id, caller.Role)
		if errors.Is(err, sql.ErrNoRows) {
			return http.StatusNotFound, nil, errors.New("message not found or not visible to this role")
		}
		if err != nil {
			return http.StatusInternalServerError, nil, errors.New("message unavailable")
		}
		return http.StatusOK, newMessageJSON(message), nil
	})
	handle("POST /v1/messages", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			To       string `json:"to"`
			Body     string `json:"body"`
			Campaign string `json:"campaign"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		if input.Campaign != "" {
			if err := workflow.ValidateWorkflowID(input.Campaign); err != nil {
				return http.StatusBadRequest, nil, err
			}
		}
		message, err := sendMessage(ctx, store, org, caller.Role, input.To, input.Body, input.Campaign)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		return hubRecordWrite(ctx, store, caller, "message.send", message)
	})
	handle("POST /v1/messages/{id}/reply", func(ctx context.Context, caller hub.Caller, org config.OrgConfig, r *http.Request) (int, any, error) {
		id, err := hubMessageID(r)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		var input struct {
			Body string `json:"body"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		message, err := replyMessage(ctx, store, org, caller.Role, id, input.Body)
		if err != nil {
			return http.StatusBadRequest, nil, err
		}
		return hubRecordWrite(ctx, store, caller, "message.reply", message)
	})
	return mux
}

// hubRecordWrite records which machine and role made a write. The message is
// already saved, so a failure to record is reported but does not undo it.
func hubRecordWrite(ctx context.Context, store *db.Store, caller hub.Caller, action string, message db.Message) (int, any, error) {
	if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, action, "message:"+strconv.FormatInt(message.ID, 10), time.Now().UTC()); err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("message %d saved, but recording the hub write failed", message.ID)
	}
	return http.StatusCreated, newMessageJSON(message), nil
}

func hubPage(r *http.Request) (int64, int, error) {
	query := r.URL.Query()
	before, limit := int64(0), 20
	var err error
	if raw := query.Get("before"); raw != "" {
		if before, err = strconv.ParseInt(raw, 10, 64); err != nil || before < 0 {
			return 0, 0, errors.New("before must be a message ID >= 0")
		}
	}
	if raw := query.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 100 {
			return 0, 0, errors.New("limit must be 1-100")
		}
	}
	return before, limit, nil
}

func hubMessageID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid message ID")
	}
	return id, nil
}

func decodeHubBody(r *http.Request, value any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("request body: %w", err)
	}
	if decoder.More() {
		return errors.New("request body: one JSON object expected")
	}
	return nil
}

// startDaemonHub serves the hub when [hub] listen is set and returns its stop
// func. A hub that cannot start is reported and skipped: the daemon's local
// work does not depend on it, and a refused start would stop that work too.
func startDaemonHub(home string, stdout io.Writer) func() {
	paths, err := pathsFromFlag(home)
	if err != nil {
		writeLine(stdout, "hub: not started: %v", err)
		return func() {}
	}
	cfg, err := config.LoadHubConfig(paths)
	if err != nil {
		writeLine(stdout, "hub: not started: %v", err)
		return func() {}
	}
	if cfg.Listen == "" {
		return func() {}
	}
	server, store, err := startHub(paths, cfg)
	if err != nil {
		writeLine(stdout, "hub: not started: %v", err)
		return func() {}
	}
	writeLine(stdout, "hub: serving on https://%s (client certificates from %s)", server.Addr(), hub.Dir(paths.Home))
	return func() {
		server.Close()
		_ = store.Close()
	}
}

func startHub(paths config.Paths, cfg config.HubConfig) (*hub.Server, *db.Store, error) {
	address, err := config.ValidateHubListen(cfg.Listen)
	if err != nil {
		return nil, nil, err
	}
	authority, err := hub.LoadOrCreateAuthority(hub.Dir(paths.Home))
	if err != nil {
		return nil, nil, err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", address.String())
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	hosts := append([]string{address.Addr().String()}, cfg.Advertise...)
	handler := hub.Authenticate(store.ActiveHubMachineByFingerprint, time.Now, config.TailscalePrefixes, hubAPI(paths, store))
	server, err := hub.Serve(listener, authority, hosts, handler)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return server, store, nil
}
