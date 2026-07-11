package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/activepieces"
	"github.com/jerryfane/gitmoot/internal/cli/style"
)

const (
	defaultActivepiecesPort    = 8080
	defaultActivepiecesEmail   = "admin@gitmoot.local"
	defaultActivepiecesProject = "gitmoot-activepieces"
	activepiecesConnectionID   = "gitmoot-bridge"
	activepiecesBridgePIDFile  = "bridge.pid"
)

func runActivepieces(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printActivepiecesUsage(stdout)
		return 0
	}
	switch args[0] {
	case "setup":
		return runActivepiecesSetup(args[1:], stdout, stderr)
	case "down":
		return runActivepiecesDown(args[1:], stdout, stderr)
	case "templates":
		return runActivepiecesTemplates(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown activepieces command %q\n\n", args[0])
		printActivepiecesUsage(stderr)
		return 2
	}
}

func printActivepiecesUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot activepieces setup [flags]")
	fmt.Fprintln(w, "  gitmoot activepieces down [--volumes] [--stop-bridge] [flags]")
	fmt.Fprintln(w, "  gitmoot activepieces templates list")
	fmt.Fprintln(w, "  gitmoot activepieces templates import [flags] [id...]")
}

func runActivepiecesSetup(args []string, stdout, stderr io.Writer) int {
	defaultBridgeAddr, _, defaultBridgeURL := resolveBridgeBind(runtime.GOOS)
	fs := flag.NewFlagSet("activepieces setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	apURL := fs.String("url", "", "existing local Activepieces URL; skips Docker bootstrap")
	port := fs.Int("port", defaultActivepiecesPort, "local Activepieces port")
	pieceVersion := fs.String("piece-version", "", "Gitmoot piece version; defaults to the latest npm release")
	email := fs.String("email", defaultActivepiecesEmail, "Activepieces admin email")
	password := fs.String("password", "", "Activepieces admin password; generated when omitted")
	bridgeAddr := fs.String("bridge-addr", defaultBridgeAddr, "Gitmoot bridge listen address")
	bridgeURL := fs.String("bridge-url", defaultBridgeURL, "URL Activepieces uses to reach the Gitmoot bridge")
	noBridgeSpawn := fs.Bool("no-bridge-spawn", false, "require an existing Gitmoot bridge instead of starting one")
	recreateConnection := fs.Bool("recreate-connection", false, "replace an existing gitmoot-bridge connection")
	composeProject := fs.String("compose-project", defaultActivepiecesProject, "Docker Compose project name")
	noTemplates := fs.Bool("no-templates", false, "skip starter template import")
	yes := fs.Bool("yes", false, "accept setup prompts")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "activepieces setup does not accept positional arguments")
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(stderr, "activepieces setup: --port must be between 1 and 65535")
		return 2
	}
	if strings.TrimSpace(*email) == "" {
		fmt.Fprintln(stderr, "activepieces setup: --email is required")
		return 2
	}
	if strings.TrimSpace(*composeProject) == "" {
		fmt.Fprintln(stderr, "activepieces setup: --compose-project is required")
		return 2
	}

	bridgeAddrSet := activepiecesFlagWasSet(fs, "bridge-addr")
	bridgeURLSet := activepiecesFlagWasSet(fs, "bridge-url")
	if bridgeAddrSet && !bridgeURLSet {
		_, bridgePort, err := net.SplitHostPort(strings.TrimSpace(*bridgeAddr))
		if err != nil {
			fmt.Fprintf(stderr, "activepieces setup: invalid --bridge-addr: %v\n", err)
			return 2
		}
		*bridgeURL = "http://host.docker.internal:" + bridgePort
	}
	if _, _, err := net.SplitHostPort(strings.TrimSpace(*bridgeAddr)); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: invalid --bridge-addr: %v\n", err)
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces setup: resolve paths: %v\n", err)
		return 1
	}
	stackDir := filepath.Join(paths.Home, "activepieces")
	if err := os.MkdirAll(stackDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: create stack directory: %v\n", err)
		return 1
	}
	if err := os.Chmod(stackDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: secure stack directory: %v\n", err)
		return 1
	}
	tokenPath, err := ensureBridgeToken(paths, false)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces setup: ensure bridge token: %v\n", err)
		return 1
	}
	bridgeToken, err := readBridgeToken(tokenPath)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces setup: read bridge token: %v\n", err)
		return 1
	}

	allowRemote := bridgeAddrNeedsAllowRemote(*bridgeAddr)
	if err := ensureActivepiecesBridge(*home, stackDir, *bridgeAddr, allowRemote, *noBridgeSpawn); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}
	if allowRemote {
		fmt.Fprintf(stdout, "Bridge is reachable by local containers on %s (bearer-token protected).\n", *bridgeAddr)
	}

	targetURL := strings.TrimRight(strings.TrimSpace(*apURL), "/")
	if targetURL == "" {
		targetURL = fmt.Sprintf("http://localhost:%d", *port)
		secrets, err := activepieces.NewSecrets()
		if err != nil {
			fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
			return 1
		}
		if err := activepieces.WriteStack(stackDir, secrets, *port, targetURL); err != nil {
			fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
			return 1
		}
		if err := activepieces.ComposeUp(context.Background(), stackDir, *composeProject); err != nil {
			fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
			return 1
		}
	}
	// The piece install is synchronous (npm tarball download + engine metadata
	// extraction) and can exceed the default 30s, so give the client room.
	client, err := activepieces.NewClient(targetURL, &http.Client{Timeout: 180 * time.Second})
	if err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}
	if err := client.WaitHealthy(context.Background(), 180*time.Second); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}

	credentialsPath := filepath.Join(stackDir, "ADMIN_CREDENTIALS.txt")
	adminPassword, generated, err := resolveAdminPassword(credentialsPath, strings.TrimSpace(*email), *password)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}
	token, projectID, createdAccount, err := client.SignUpOrIn(context.Background(), strings.TrimSpace(*email), adminPassword)
	if err != nil {
		if generated {
			fmt.Fprintf(stderr, "activepieces setup: %v; if this account already exists, pass its password with --password\n", err)
		} else {
			fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		}
		return 1
	}
	printedCredentials := false
	if generated || createdAccount {
		createdFile, err := writeAdminCredentialsOnce(credentialsPath, strings.TrimSpace(*email), adminPassword)
		if err != nil {
			fmt.Fprintf(stderr, "activepieces setup: write admin credentials: %v\n", err)
			return 1
		}
		if createdFile {
			fmt.Fprintf(stdout, "Activepieces admin credentials (also saved at %s):\n", credentialsPath)
			fmt.Fprintf(stdout, "  email: %s\n", strings.TrimSpace(*email))
			fmt.Fprintf(stdout, "  password: %s\n", adminPassword)
			printedCredentials = true
		}
	}

	resolvedPieceVersion := strings.TrimSpace(*pieceVersion)
	if resolvedPieceVersion == "" {
		resolvedPieceVersion, err = activepieces.ResolveLatestPieceVersion(context.Background())
		if err != nil {
			// Activepieces 0.82 requires an exact pieceVersion, so there is no
			// safe empty fallback: ask the user to pin one.
			fmt.Fprintf(stderr, "activepieces setup: could not resolve the latest @gitmoot/piece-gitmoot version from npm (%v); re-run with --piece-version X.Y.Z\n", err)
			return 1
		}
	}
	if !isExactPieceVersion(resolvedPieceVersion) {
		fmt.Fprintf(stderr, "activepieces setup: piece version %q must be exact like 0.1.2 (no ~ or ^)\n", resolvedPieceVersion)
		return 2
	}
	if err := client.InstallPiece(context.Background(), token, "@gitmoot/piece-gitmoot", resolvedPieceVersion); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}
	if err := client.UpsertBridgeConnection(context.Background(), token, projectID, activepiecesConnectionID, strings.TrimRight(*bridgeURL, "/"), bridgeToken, *recreateConnection); err != nil {
		fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
		return 1
	}

	var imported []importedActivepiecesFlow
	if !*noTemplates && confirmStarterTemplates(stdout, *yes) {
		imported, err = importActivepiecesTemplates(context.Background(), client, token, projectID, nil, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "activepieces setup: %v\n", err)
			return 1
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Activepieces is ready.")
	fmt.Fprintf(stdout, "  Open: %s\n", targetURL)
	if createdAccount && !printedCredentials {
		fmt.Fprintf(stdout, "  Admin: %s (credentials saved at %s)\n", strings.TrimSpace(*email), credentialsPath)
	}
	fmt.Fprintf(stdout, "  Piece: @gitmoot/piece-gitmoot@%s\n", resolvedPieceVersion)
	fmt.Fprintf(stdout, "  Connection: %s -> %s\n", activepiecesConnectionID, strings.TrimRight(*bridgeURL, "/"))
	if len(imported) > 0 {
		fmt.Fprintln(stdout, "  Starter flows:")
		for _, flow := range imported {
			fmt.Fprintf(stdout, "    %s: %s\n", flow.DisplayName, flow.URL)
		}
	}
	fmt.Fprintln(stdout, "  Next: authorize Gmail or another mailbox provider. See docs/gmail.md.")
	return 0
}

// exactPieceVersionPattern matches Activepieces' ExactVersionType for a
// REGISTRY install: a bare semver with no range prefix.
var exactPieceVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func isExactPieceVersion(v string) bool {
	return exactPieceVersionPattern.MatchString(strings.TrimSpace(v))
}

func activepiecesFlagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func resolveBridgeBind(goos string) (addr string, allowRemote bool, apReachableURL string) {
	if goos == "linux" {
		return "172.17.0.1:8791", true, "http://host.docker.internal:8791"
	}
	return defaultBridgeAddr, false, "http://host.docker.internal:8791"
}

func bridgeAddrNeedsAllowRemote(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	return err == nil && !bridgeHostIsLoopback(host)
}

func ensureActivepiecesBridge(home, stackDir, addr string, allowRemote, noSpawn bool) error {
	if bridgeTCPReady(addr, 300*time.Millisecond) {
		return nil
	}
	if noSpawn {
		return fmt.Errorf("no Gitmoot bridge is listening on %s", addr)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find gitmoot executable: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(stackDir, "bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open bridge log: %w", err)
	}
	defer logFile.Close()
	args := []string{"bridge", "serve", "--addr", addr}
	if allowRemote {
		args = append(args, "--allow-remote")
	}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Gitmoot bridge: %w", err)
	}
	if err := writeActivepiecesBridgePID(stackDir, cmd.Process.Pid, addr); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("write Gitmoot bridge pid file: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		_ = os.Remove(filepath.Join(stackDir, activepiecesBridgePIDFile))
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("release Gitmoot bridge process: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if bridgeTCPReady(addr, 500*time.Millisecond) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("Gitmoot bridge did not start on %s; inspect %s", addr, filepath.Join(stackDir, "bridge.log"))
}

type activepiecesBridgePID struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func writeActivepiecesBridgePID(stackDir string, pid int, addr string) error {
	if pid <= 0 || strings.TrimSpace(addr) == "" {
		return errors.New("invalid Gitmoot bridge process metadata")
	}
	body, err := json.Marshal(activepiecesBridgePID{PID: pid, Addr: strings.TrimSpace(addr)})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(stackDir, ".bridge-pid-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(stackDir, activepiecesBridgePIDFile)
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func readActivepiecesBridgePID(stackDir string) (activepiecesBridgePID, error) {
	raw, err := os.ReadFile(filepath.Join(stackDir, activepiecesBridgePIDFile))
	if err != nil {
		return activepiecesBridgePID{}, err
	}
	var process activepiecesBridgePID
	if err := json.Unmarshal(raw, &process); err != nil {
		return activepiecesBridgePID{}, err
	}
	process.Addr = strings.TrimSpace(process.Addr)
	if process.PID <= 0 || process.Addr == "" {
		return activepiecesBridgePID{}, errors.New("invalid Gitmoot bridge process metadata")
	}
	return process, nil
}

func inspectActivepiecesBridgePID(pid int) (alive, verified bool) {
	if pid <= 0 {
		return false, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, false
		}
		return true, false
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return true, false
	}
	return true, isGitmootBridgeCmdline(raw)
}

func isGitmootBridgeCmdline(raw []byte) bool {
	cmdline := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "\x00", " ")), " ")
	return strings.Contains(cmdline, "bridge serve")
}

func stopActivepiecesBridgePID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

func reconcileActivepiecesBridgePID(
	stackDir string,
	stop bool,
	stdout io.Writer,
	inspect func(int) (alive, verified bool),
	terminate func(int) error,
) error {
	path := filepath.Join(stackDir, activepiecesBridgePIDFile)
	process, err := readActivepiecesBridgePID(stackDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if stop {
				fmt.Fprintln(stdout, "There was nothing to stop: no verified gitmoot bridge started by setup was found.")
			}
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove stale Gitmoot bridge pid file: %w", removeErr)
		}
		if stop {
			fmt.Fprintln(stdout, "There was nothing to stop: no verified gitmoot bridge started by setup was found.")
		}
		return nil
	}
	alive, verified := inspect(process.PID)
	if !alive {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale Gitmoot bridge pid file: %w", err)
		}
		if stop {
			fmt.Fprintln(stdout, "There was nothing to stop: no verified gitmoot bridge started by setup was found.")
		}
		return nil
	}
	if !verified {
		if !stop {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove unverified Gitmoot bridge pid file: %w", err)
		}
		fmt.Fprintln(stdout, "There was nothing to stop: no verified gitmoot bridge started by setup was found.")
		return nil
	}
	if !stop {
		fmt.Fprintf(stdout, "The gitmoot bridge that setup started is still running (pid %d on %s). Stop it with: gitmoot activepieces down --stop-bridge\n", process.PID, process.Addr)
		return nil
	}
	if err := terminate(process.PID); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = os.Remove(path)
			fmt.Fprintln(stdout, "There was nothing to stop: no verified gitmoot bridge started by setup was found.")
			return nil
		}
		return fmt.Errorf("stop Gitmoot bridge pid %d: %w", process.PID, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Gitmoot bridge pid file: %w", err)
	}
	fmt.Fprintf(stdout, "Stopped the gitmoot bridge that setup started (pid %d on %s).\n", process.PID, process.Addr)
	return nil
}

func bridgeTCPReady(addr string, timeout time.Duration) bool {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}
	return conn.Close() == nil
}

func resolveAdminPassword(path, email, supplied string) (password string, generated bool, err error) {
	if supplied != "" {
		return supplied, false, nil
	}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		values := parseCredentialFile(raw)
		if values["Email"] == email && values["Password"] != "" {
			return values["Password"], false, nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", false, fmt.Errorf("read saved admin credentials: %w", readErr)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", false, fmt.Errorf("generate admin password: %w", err)
	}
	return hex.EncodeToString(raw), true, nil
}

func parseCredentialFile(raw []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

func writeAdminCredentialsOnce(path, email, password string) (bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	body := fmt.Sprintf("Email=%s\nPassword=%s\n", email, password)
	if _, err := io.WriteString(file, body); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, os.Chmod(path, 0o600)
}

func confirmStarterTemplates(stdout io.Writer, yes bool) bool {
	if yes || !style.IsTerminal(stdout) || !style.IsTerminal(os.Stdin) {
		return true
	}
	fmt.Fprint(stdout, "Import starter templates? [Y/n] ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func runActivepiecesDown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("activepieces down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	composeProject := fs.String("compose-project", defaultActivepiecesProject, "Docker Compose project name")
	volumes := fs.Bool("volumes", false, "also remove Activepieces data volumes")
	stopBridge := fs.Bool("stop-bridge", false, "also stop the Gitmoot bridge started by setup")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "activepieces down does not accept positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces down: resolve paths: %v\n", err)
		return 1
	}
	stackDir := filepath.Join(paths.Home, "activepieces")
	if err := activepieces.ComposeDown(context.Background(), stackDir, *composeProject, *volumes); err != nil {
		fmt.Fprintf(stderr, "activepieces down: %v\n", err)
		return 1
	}
	if *volumes {
		fmt.Fprintln(stdout, "Activepieces stopped and its local data volumes were removed.")
	} else {
		fmt.Fprintln(stdout, "Activepieces stopped. Local data volumes were preserved.")
	}
	if err := reconcileActivepiecesBridgePID(stackDir, *stopBridge, stdout, inspectActivepiecesBridgePID, stopActivepiecesBridgePID); err != nil {
		fmt.Fprintf(stderr, "activepieces down: %v\n", err)
		return 1
	}
	return 0
}

func runActivepiecesTemplates(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printActivepiecesTemplatesUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runActivepiecesTemplatesList(args[1:], stdout, stderr)
	case "import":
		return runActivepiecesTemplatesImport(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown activepieces templates command %q\n\n", args[0])
		printActivepiecesTemplatesUsage(stderr)
		return 2
	}
}

func printActivepiecesTemplatesUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot activepieces templates list")
	fmt.Fprintln(w, "  gitmoot activepieces templates import [flags] [id...]")
}

func runActivepiecesTemplatesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("activepieces templates list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "activepieces templates list does not accept positional arguments")
		return 2
	}
	templates, err := activepieces.Templates()
	if err != nil {
		fmt.Fprintf(stderr, "activepieces templates list: %v\n", err)
		return 1
	}
	for _, template := range templates {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", template.ID, template.DisplayName, template.Description)
	}
	return 0
}

func runActivepiecesTemplatesImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("activepieces templates import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	apURL := fs.String("url", "", "Activepieces URL")
	port := fs.Int("port", defaultActivepiecesPort, "local Activepieces port")
	email := fs.String("email", defaultActivepiecesEmail, "Activepieces admin email")
	password := fs.String("password", "", "Activepieces admin password")
	parsedArgs, err := reorderFlagArgs(args, map[string]struct{}{
		"home": {}, "url": {}, "port": {}, "email": {}, "password": {},
	}, nil)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces templates import: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(stderr, "activepieces templates import: --port must be between 1 and 65535")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces templates import: resolve paths: %v\n", err)
		return 1
	}
	adminPassword := *password
	if adminPassword == "" {
		credentialsPath := filepath.Join(paths.Home, "activepieces", "ADMIN_CREDENTIALS.txt")
		raw, err := os.ReadFile(credentialsPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(stderr, "activepieces templates import: --password is required when no saved admin credentials exist")
				return 2
			}
			fmt.Fprintf(stderr, "activepieces templates import: read saved admin credentials: %v\n", err)
			return 1
		}
		values := parseCredentialFile(raw)
		if values["Email"] != strings.TrimSpace(*email) || values["Password"] == "" {
			fmt.Fprintln(stderr, "activepieces templates import: saved credentials do not match --email; pass --password")
			return 2
		}
		adminPassword = values["Password"]
	}
	targetURL := strings.TrimRight(strings.TrimSpace(*apURL), "/")
	if targetURL == "" {
		targetURL = "http://localhost:" + strconv.Itoa(*port)
	}
	client, err := activepieces.NewClient(targetURL, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintf(stderr, "activepieces templates import: %v\n", err)
		return 1
	}
	token, projectID, _, err := client.SignUpOrIn(context.Background(), strings.TrimSpace(*email), adminPassword)
	if err != nil {
		fmt.Fprintf(stderr, "activepieces templates import: %v\n", err)
		return 1
	}
	if _, err := importActivepiecesTemplates(context.Background(), client, token, projectID, fs.Args(), stdout); err != nil {
		fmt.Fprintf(stderr, "activepieces templates import: %v\n", err)
		return 1
	}
	return 0
}

type importedActivepiecesFlow struct {
	DisplayName string
	URL         string
}

func importActivepiecesTemplates(ctx context.Context, client *activepieces.Client, token, projectID string, ids []string, stdout io.Writer) ([]importedActivepiecesFlow, error) {
	templates, err := activepieces.SelectTemplates(ids)
	if err != nil {
		return nil, err
	}
	existing, err := client.ListFlows(ctx, token, projectID)
	if err != nil {
		return nil, err
	}
	existingNames := make(map[string]bool, len(existing))
	for _, flow := range existing {
		existingNames[flow.DisplayName] = true
	}
	imported := make([]importedActivepiecesFlow, 0, len(templates))
	for _, template := range templates {
		if existingNames[template.DisplayName] {
			fmt.Fprintf(stdout, "Skipped %s: a flow with that name already exists.\n", template.DisplayName)
			continue
		}
		flowID, err := client.CreateFlow(ctx, token, projectID, template.DisplayName)
		if err != nil {
			return imported, fmt.Errorf("import template %s: %w", template.ID, err)
		}
		if err := client.ImportFlow(ctx, token, flowID, template.Flow); err != nil {
			_ = client.DeleteFlow(ctx, token, flowID)
			return imported, fmt.Errorf("import template %s: %w", template.ID, err)
		}
		flowURL := fmt.Sprintf("%s/projects/%s/flows/%s", client.BaseURL(), projectID, flowID)
		fmt.Fprintf(stdout, "Imported %s: %s\n", template.DisplayName, flowURL)
		imported = append(imported, importedActivepiecesFlow{DisplayName: template.DisplayName, URL: flowURL})
		existingNames[template.DisplayName] = true
	}
	return imported, nil
}
