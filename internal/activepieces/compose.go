package activepieces

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed compose.yaml
var composeYAML []byte

var (
	// ErrLatestPieceVersionUnavailable lets setup continue without a pinned
	// version so Activepieces can resolve the current registry release.
	ErrLatestPieceVersionUnavailable = errors.New("latest Gitmoot piece version is unavailable")
	pieceRegistryURL                 = "https://registry.npmjs.org/-/package/@gitmoot%2Fpiece-gitmoot/dist-tags"
	pieceRegistryHTTPClient          = &http.Client{Timeout: 8 * time.Second}
)

type Secrets struct {
	EncryptionKey    string
	JwtSecret        string
	PostgresDB       string
	PostgresUser     string
	PostgresPassword string
}

func NewSecrets() (Secrets, error) {
	encryptionKey, err := randomHex(16)
	if err != nil {
		return Secrets{}, fmt.Errorf("generate encryption key: %w", err)
	}
	jwtSecret, err := randomHex(32)
	if err != nil {
		return Secrets{}, fmt.Errorf("generate JWT secret: %w", err)
	}
	postgresPassword, err := randomHex(24)
	if err != nil {
		return Secrets{}, fmt.Errorf("generate Postgres password: %w", err)
	}
	return Secrets{
		EncryptionKey:    encryptionKey,
		JwtSecret:        jwtSecret,
		PostgresDB:       "activepieces",
		PostgresUser:     "activepieces",
		PostgresPassword: postgresPassword,
	}, nil
}

func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func WriteStack(dir string, secrets Secrets, port int, frontendURL string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("Activepieces port must be between 1 and 65535")
	}
	if err := validateEnvValue("AP_FRONTEND_URL", frontendURL); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Activepieces directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure Activepieces directory: %w", err)
	}
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, composeYAML, 0o644); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}
	if err := os.Chmod(composePath, 0o644); err != nil {
		return fmt.Errorf("set compose file permissions: %w", err)
	}

	envPath := filepath.Join(dir, ".env")
	values := map[string]string{}
	envExists := false
	if raw, err := os.ReadFile(envPath); err == nil {
		values = parseEnv(raw)
		envExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Activepieces environment: %w", err)
	}
	defaults := map[string]string{
		"AP_ENCRYPTION_KEY":    secrets.EncryptionKey,
		"AP_JWT_SECRET":        secrets.JwtSecret,
		"AP_POSTGRES_DATABASE": secrets.PostgresDB,
		"AP_POSTGRES_USERNAME": secrets.PostgresUser,
		"AP_POSTGRES_PASSWORD": secrets.PostgresPassword,
	}
	for key, value := range defaults {
		if strings.TrimSpace(values[key]) == "" {
			values[key] = value
		}
	}
	values["AP_PORT"] = strconv.Itoa(port)
	values["AP_FRONTEND_URL"] = strings.TrimSpace(frontendURL)

	keys := []string{
		"AP_PORT",
		"AP_FRONTEND_URL",
		"AP_ENCRYPTION_KEY",
		"AP_JWT_SECRET",
		"AP_POSTGRES_DATABASE",
		"AP_POSTGRES_USERNAME",
		"AP_POSTGRES_PASSWORD",
	}
	var body strings.Builder
	for _, key := range keys {
		if err := validateEnvValue(key, values[key]); err != nil {
			return err
		}
		fmt.Fprintf(&body, "%s=%s\n", key, values[key])
	}
	if !envExists {
		if err := writeSecretFileCreateOnly(envPath, []byte(body.String())); err == nil {
			return nil
		} else if errors.Is(err, os.ErrExist) {
			// Another setup won the create race. Re-read its secrets instead of
			// replacing them with this process's freshly generated values.
			return WriteStack(dir, secrets, port, frontendURL)
		} else {
			return fmt.Errorf("write Activepieces environment: %w", err)
		}
	}
	if err := writeSecretFile(envPath, []byte(body.String())); err != nil {
		return fmt.Errorf("write Activepieces environment: %w", err)
	}
	return nil
}

func validateEnvValue(key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a newline", key)
	}
	return nil
}

func parseEnv(raw []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

func writeSecretFile(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".activepieces-env-*")
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func writeSecretFileCreateOnly(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return os.Chmod(path, 0o600)
}

func ComposeUp(ctx context.Context, dir, project string) error {
	args := composeArgs(dir, project, "up", "-d")
	return runDockerCompose(ctx, args)
}

func ComposeDown(ctx context.Context, dir, project string, volumes bool) error {
	args := composeArgs(dir, project, "down")
	if volumes {
		args = append(args, "--volumes")
	}
	return runDockerCompose(ctx, args)
}

func composeArgs(dir, project string, command ...string) []string {
	args := []string{
		"compose",
		"-p", project,
		"--env-file", filepath.Join(dir, ".env"),
		"-f", filepath.Join(dir, "compose.yaml"),
	}
	return append(args, command...)
}

func runDockerCompose(ctx context.Context, args []string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("Docker is required for a local Activepieces; install Docker or pass --url <existing-ap>")
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("docker compose: %w", err)
		}
		return fmt.Errorf("docker compose: %w: %s", err, detail)
	}
	return nil
}

func ResolveLatestPieceVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pieceRegistryURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLatestPieceVersionUnavailable, err)
	}
	resp, err := pieceRegistryHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLatestPieceVersionUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLatestPieceVersionUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: npm returned %s: %s", ErrLatestPieceVersionUnavailable, resp.Status, strings.TrimSpace(string(body)))
	}
	var tags struct {
		Latest string `json:"latest"`
	}
	if err := json.Unmarshal(body, &tags); err != nil {
		return "", fmt.Errorf("%w: decode npm response: %v", ErrLatestPieceVersionUnavailable, err)
	}
	if strings.TrimSpace(tags.Latest) == "" {
		return "", fmt.Errorf("%w: npm response has no latest tag", ErrLatestPieceVersionUnavailable)
	}
	return strings.TrimSpace(tags.Latest), nil
}
